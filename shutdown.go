package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gateway/internal/health"
)

// gracefulShutdown 分三步下线，每一步都有明确目的：
//
//  1. 进入 draining：/readyz 立即返回 503，让负载均衡把本实例摘出去；
//  2. 等待 drain：给 LB / kube-proxy / Endpoints 传播"本实例已摘除"留出时间。
//     这段时间里服务仍正常处理请求（新请求也还会来一些，照常服务）；
//  3. srv.Shutdown：不再接受新连接，等正在处理的请求（尤其是长流式响应）跑完，最多等 grace。
//     超时则强制关闭剩余连接，并返回错误，调用方应以非零状态退出。
//
// 注意 K8s 的 terminationGracePeriodSeconds 必须 >= drain + grace，
// 否则还没等我们优雅结束，kubelet 就先 SIGKILL 了。
func gracefulShutdown(srv *http.Server, d *health.Drainer, drain, grace time.Duration, log *slog.Logger) error {
	d.Start()
	log.Info("开始下线：/readyz 已返回 503，等待负载均衡摘流量", "drain", drain.String())
	if drain > 0 {
		time.Sleep(drain)
	}

	log.Info("停止接受新连接，等待在途请求完成", "grace", grace.String())
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close() // 宽限期用完仍有请求未结束：强制断开，避免进程悬挂
		return fmt.Errorf("优雅关闭超时，已强制断开剩余连接: %w", err)
	}
	return nil
}
