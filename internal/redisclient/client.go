// Package redisclient 负责创建和管理到 Redis 的连接。
// 单独成包的原因：连接池参数、健康检查这些"基础设施代码"跟业务逻辑
// （限流算法、配额算法）分开，main.go 只管调用 New()，不关心连接细节。
package redisclient

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Config struct {
	Addr         string // 例如 "localhost:6379"
	Password     string
	DB           int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
}

// FromSettings 用"配置文件里的原始字段"构造客户端，供 main.go 和 cmd/seed
// 共用同一套映射逻辑。
//
// 为什么要多这一层：如果两处调用方各自把 ms 转成 time.Duration，
// 很容易出现"网关连 Redis 用 2 秒超时、种子工具用默认超时"这种不一致，
// 排查起来很费时间。把映射收在一个地方，两个可执行文件的行为就必然一致。
// 传 0 表示不设置，交给 go-redis 使用它自己的默认值。
func FromSettings(addr, password string, db, dialTimeoutMs, readTimeoutMs, writeTimeoutMs, poolSize int) (*redis.Client, error) {
	return New(Config{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  time.Duration(dialTimeoutMs) * time.Millisecond,
		ReadTimeout:  time.Duration(readTimeoutMs) * time.Millisecond,
		WriteTimeout: time.Duration(writeTimeoutMs) * time.Millisecond,
		PoolSize:     poolSize,
	})
}

// New 创建 Redis 客户端并立即做一次 PING 验证连接是否可用。
// 学习点：企业级代码里，"创建连接对象"和"验证连接真的能用"是两件事。
// go-redis 的 NewClient 只是创建了一个客户端结构体，不会立刻真正连接，
// 如果配置错了（比如地址写错），要等到第一次真正用的时候才会报错。
// 在启动阶段主动 Ping 一次，能让配置错误在服务启动时就暴露出来，
// 而不是等到线上流量进来才发现连不上 Redis。
func New(cfg Config) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		PoolSize:     cfg.PoolSize,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("连接 Redis 失败 (%s): %w", cfg.Addr, err)
	}

	return client, nil
}
