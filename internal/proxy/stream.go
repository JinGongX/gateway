package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// 一、客户端方向：滚动写超时（替代 http.Server.WriteTimeout）
// ---------------------------------------------------------------------------

// DeadlineWriter 包装 http.ResponseWriter，在每次写之前把"写截止时间"往后推。
//
// 为什么需要它：http.Server.WriteTimeout 是从"请求头读完"开始计时的【绝对】期限。
// 设成 300s，一次 301 秒的长对话就会在第 300 秒被直接掐断，
// 而且此时响应头早已发出，客户端看到的只是一个莫名其妙的断流。
// 但如果把 WriteTimeout 设成 0（关闭），一个"连上就不再读数据"的慢客户端
// 就能让网关的 goroutine 和上游连接一直被占住。
//
// 折中方案是「滚动」超时：只要还在持续写出数据，就不断续期；
// 如果某一次写在 idle 内没完成（客户端不读了），才判定为卡死。
// 另外用 maxTotal 给单个流设一个总时长硬上限，防止无限生成占住 GPU。
//
// 依赖 http.ResponseController：底层 writer 不支持 SetWriteDeadline 时
// （比如 httptest.ResponseRecorder，或者包装链上缺少 Unwrap 的中间层），
// 会静默降级为"不设写超时"，调用方可以通过 Unsupported() 得知并告警。
type DeadlineWriter struct {
	http.ResponseWriter
	rc          *http.ResponseController
	idle        time.Duration
	hard        time.Time // 零值表示没有总时长上限
	unsupported bool
}

// NewDeadlineWriter 创建包装器。idle <= 0 表示不做滚动超时；maxTotal <= 0 表示不设总时长上限。
func NewDeadlineWriter(w http.ResponseWriter, idle, maxTotal time.Duration) *DeadlineWriter {
	d := &DeadlineWriter{
		ResponseWriter: w,
		rc:             http.NewResponseController(w),
		idle:           idle,
	}
	if maxTotal > 0 {
		d.hard = time.Now().Add(maxTotal)
	}
	return d
}

// nextDeadline 计算这一次写之前应设置的截止时间；返回 false 表示不需要设置。
func (d *DeadlineWriter) nextDeadline() (time.Time, bool) {
	var dl time.Time
	if d.idle > 0 {
		dl = time.Now().Add(d.idle)
	}
	if !d.hard.IsZero() && (dl.IsZero() || dl.After(d.hard)) {
		dl = d.hard
	}
	return dl, !dl.IsZero()
}

func (d *DeadlineWriter) extend() {
	if d.unsupported {
		return
	}
	dl, ok := d.nextDeadline()
	if !ok {
		return
	}
	if err := d.rc.SetWriteDeadline(dl); err != nil && errors.Is(err, http.ErrNotSupported) {
		d.unsupported = true
	}
}

func (d *DeadlineWriter) WriteHeader(code int) {
	d.extend()
	d.ResponseWriter.WriteHeader(code)
}

func (d *DeadlineWriter) Write(p []byte) (int, error) {
	d.extend()
	return d.ResponseWriter.Write(p)
}

// Unwrap 让 http.ResponseController 能穿过本包装器找到 Flush / Hijack 等能力。
// ReverseProxy 正是通过它来 flush 的，缺了它流式转发就会失效。
func (d *DeadlineWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

// Unsupported 报告底层 writer 是否不支持设置写超时（此时保护没有生效）。
func (d *DeadlineWriter) Unsupported() bool { return d.unsupported }

// ---------------------------------------------------------------------------
// 二、上游方向：响应体空闲超时
// ---------------------------------------------------------------------------

// idleCancelKey 用来把"取消本次上游请求"的函数从 Rewrite 带到 ModifyResponse。
type idleCancelKey struct{}

// idleTimeoutBody 包装上游响应体：如果上游在 d 内一个字节都没吐出来，就取消这次请求。
//
// 为什么需要：模型后端卡死（GPU 挂起、进程假死）时，TCP 连接依然是通的，
// 网关这边只会一直等下去——客户端的连接、网关的 goroutine、租户的并发名额全被占着。
// ResponseHeaderTimeout 只管"响应头"，管不到"流到一半不动了"。
//
// 注意计时只覆盖"等待上游数据"的时间：每次 Read 开始前启动计时，返回后立即停止。
// 如果网关因为客户端读得慢而没来得及读上游，这段时间不算上游的空闲。
type idleTimeoutBody struct {
	io.ReadCloser
	timer  *time.Timer
	d      time.Duration
	cancel context.CancelFunc
}

func newIdleTimeoutBody(rc io.ReadCloser, d time.Duration, cancel context.CancelFunc, onIdle func()) io.ReadCloser {
	b := &idleTimeoutBody{ReadCloser: rc, d: d, cancel: cancel}
	b.timer = time.AfterFunc(d, func() {
		if onIdle != nil {
			onIdle()
		}
		cancel()
	})
	b.timer.Stop() // 先停住，只在 Read 期间计时
	return b
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.d)
	n, err := b.ReadCloser.Read(p)
	b.timer.Stop()
	return n, err
}

func (b *idleTimeoutBody) Close() error {
	b.timer.Stop()
	err := b.ReadCloser.Close()
	b.cancel() // 释放这次请求派生出来的 context
	return err
}
