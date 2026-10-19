// Package auth 定义"租户"这个领域对象，以及鉴权失败的统一错误。
//
// 学习点回顾：这里用 API Key 而不是 JWT，是因为场景是"服务间长期调用"，
// 不是"用户登录"。网关需要的是查出这个租户的限流/配额配置，
// 而不是验证一个签名令牌里的 claims。两者解决的是不同层面的问题。
//
// 历史沿革：这个包曾经还包含一个内存版的 Store（map[apiKey]*Tenant），
// 租户数据搬到 PostgreSQL 之后已经删除，现在只保留领域对象和错误定义；
// 旧实现作为对照存档在 docs/legacy-inmemory-examples.md。
package auth

import "errors"

// Tenant 描述一个接入网关的租户（比如你系统里的"订单管理系统"）
type Tenant struct {
	ID       string
	Name     string
	RPS      float64 // 限流：每秒允许的请求数
	Burst    int     // 限流：令牌桶瞬时容量
	DailyCap int     // 配额：每日调用总量上限
}

// ErrTenantNotFound 表示根据给定的 API Key 找不到有效租户
// （key 不存在、已被吊销、或者所属租户被挂起都归为这一类错误）。
// pgstore.TenantStore.Lookup 和 middleware.AuthMiddleware 都依赖这个
// 统一定义，前者是"错误的生产者"，后者是"错误的消费者"。
var ErrTenantNotFound = errors.New("tenant not found")
