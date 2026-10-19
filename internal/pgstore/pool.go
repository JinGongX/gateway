// Package pgstore 负责 PostgreSQL 侧的持久化：连接池创建 + 租户/API Key 的读写。
// 跟 internal/redisclient 一样，属于"基础设施代码"单独成包，
// 业务逻辑（鉴权中间件）只依赖 TenantStore 暴露的方法。
package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool 创建连接池并立即 Ping 一次。
// 学习点：pgxpool.New 只是解析 DSN 并构造对象，不会真正建立连接；
// 启动阶段主动 Ping，能让"库连不上/密码错/库名错"这类问题在服务启动时
// 就暴露，而不是等第一个线上请求进来才报错。
func NewPool(dsn string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("创建 PostgreSQL 连接池失败: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}

	return pool, nil
}
