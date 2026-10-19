# AI 网关（企业级最小实现）

统一代理多个模型后端（Ollama / vLLM 等）的请求，提供鉴权、限流、
多租户配额、多模型路由和流式转发能力。

## 目录结构

```
gateway/
├── main.go                          # 程序入口：加载配置、初始化依赖、优雅关闭
├── engine.go                        # 组装 gin 引擎与中间件链（顺序敏感的约定都在这里）
├── engine_test.go                   # 用假实现验证中间件顺序与计费语义
├── go.mod
├── .env.example                     # 环境变量模板（复制成 .env 使用；.env 不进版本库）
├── .gitignore
├── config/
│   └── config.yaml                  # 运行时配置：只有基础组件地址 + 模型路由表，无明文密码
├── docs/
│   └── legacy-inmemory-examples.md  # 旧的内存版限流/配额实现存档（对照用，不参与编译）
├── migrations/
│   └── 0001_init_tenants.sql        # 建表脚本：sui_tenants + sui_api_keys
├── seed/
│   └── tenants.yaml                 # 种子数据：要导入数据库的租户（含明文 key，仅本地用）
├── cmd/
│   └── seed/
│       └── main.go                  # 独立命令行工具：把种子数据写入 PostgreSQL
└── internal/
    ├── config/
    │   ├── config.go                # 加载并校验 yaml 配置
    │   ├── env.go                   # .env 加载 + ${VAR} / ${VAR:-默认值} 插值
    │   └── config_test.go
    ├── auth/
    │   └── tenant.go                # 租户模型 + 鉴权失败错误定义
    ├── middleware/
    │   ├── context_keys.go          # context key 常量 + 取租户的安全辅助函数
    │   ├── auth.go                  # 【阶段二】鉴权中间件（fail-closed）
    │   ├── ratelimit.go             # 【阶段三】限流中间件（令牌桶，按租户隔离）
    │   ├── quota.go                 # 【阶段三】配额中间件（预扣 + 失败退款）
    │   ├── routing.go               # 【阶段四】多模型路由中间件
    │   ├── recovery.go              # panic 恢复，防止单个请求崩溃拖垮整个进程
    │   └── logging.go               # 请求日志 + 请求 ID 追踪
    ├── logger/
    │   └── logger.go                # 结构化日志（slog）+ 日志器挂进 context
    ├── metrics/
    │   └── metrics.go               # Prometheus 指标集合（/metrics 端点）
    ├── health/
    │   └── health.go                # 就绪探针（依赖检查 + 结果缓存）
    ├── router/
    │   └── router.go                # 模型名 -> 后端地址 查找表
    ├── ratelimit/
    │   ├── limiter.go               # Redis Lua 令牌桶（多实例共享限流状态）
    │   └── quota.go                 # Redis 每日配额计数 + 退款脚本
    ├── redisclient/
    │   └── client.go                # Redis 客户端创建（启动时 Ping 验证）
    ├── pgstore/
    │   ├── pool.go                  # PostgreSQL 连接池（启动时 Ping 验证）
    │   └── tenant_store.go          # cache-aside 租户查找 + 租户/Key 管理
    └── proxy/
        └── pool.go                  # 【阶段一】反向代理池，含流式转发配置
```

## 配置与密钥

**配置文件里不放任何明文密码。** `config/config.yaml` 里出现的都是 `${VAR}` 占位符：

```yaml
redis:
  addr: ${REDIS_ADDR:-localhost:6379}
  password: ${REDIS_PASSWORD:-}
```

- `${VAR}`：取环境变量，没配就是空字符串
- `${VAR:-默认值}`：取环境变量，没配就用默认值
- 变量来源优先级：**进程环境变量 > 仓库根目录的 .env**
  （所以 k8s Secret 注入的值一定能覆盖本地文件，不会出现"改了 Secret 却不生效"）
- 想换 .env 的位置：`GATEWAY_ENV_FILE=/etc/gateway/env go run .`
- 配置文件里出现不认识的字段会**启动直接报错**（比如残留的 `tenants:` 段），
  不会静默忽略——改配置不生效还不报错，比启动失败更难查

日志格式和级别也在配置里（`log` 节点，可用环境变量覆盖）：

| 变量 | 取值 | 说明 |
|---|---|---|
| `LOG_LEVEL` | `debug` / `info` / `warn` / `error` | 默认 `info`；排查问题临时调成 `debug` 能看到每次转发的后端地址 |
| `LOG_FORMAT` | `text` / `json` | 默认 `text`（本地人读）；接日志平台（Loki/ELK）用 `json` |

日志是**结构化**的，每条都带 request_id，可以直接按字段检索：

```
time=... level=INFO msg=request method=POST path=/api/generate status=200 latency_ms=812.4 client_ip=10.1.2.3 bytes=517 tenant=tenant-a request_id=9f1c...
```

```json
{"level":"WARN","msg":"限流检查失败，已降级放行","tenant":"tenant-a","err":"dial tcp: connection refused","request_id":"9f1c..."}
```

```bash
cp .env.example .env   # 然后填入真实的 Redis / PostgreSQL 密码
go run .               # 启动时自动加载 .env
```

## 中间件执行顺序（很重要，顺序错了会出问题）

```
请求进来
  → RequestIDMiddleware   全局：确定请求 ID（优先透传调用方的 X-Request-ID）
  → RecoveryMiddleware    全局：兜底捕获 panic
  → MetricsMiddleware     全局：处理中数量 / 状态码 / 耗时
  → LoggingMiddleware     全局：记录结构化访问日志
  ────────── 以上对 /healthz、/readyz、/metrics 和 /api/* 都生效 ──────────
  → AuthMiddleware        仅 /api/*：验证 API Key，查出租户信息（失败即拒绝）
  → RateLimitMiddleware   仅 /api/*：按租户令牌桶限流
  → RoutingMiddleware     仅 /api/*：解析 body 里的 model 字段，决定转发到哪个后端
  → QuotaMiddleware       仅 /api/*：按租户每日配额预扣，转发失败后自动退款
  → proxyHandler          真正转发请求到 Ollama/vLLM，流式响应实时转发回客户端
```

全局中间件的顺序也是有讲究的：

| 位置 | 中间件 | 为什么在这里 |
|---|---|---|
| 1 | RequestID | 后面所有日志都要靠它拿 request_id；同时把日志器挂进请求 context |
| 2 | Recovery | 必须"包住"后面所有中间件，连我们自己写的日志代码出 panic 都能兜住。代价：panic 会跳过 Metrics/Logging 的收尾代码，所以 Recovery 自己补记一次 500 与 `gateway_panics_total` |
| 3 | Metrics | 耗时统计要覆盖后续全部环节；且不统计 `/metrics` 自己（否则抓取请求会把 `inflight` 永远抬成 1） |
| 4 | Logging | 等处理完再写日志，才能拿到最终状态码；级别按状态码选择（5xx→Error、4xx→Warn、其余→Info） |

`/api/*` 内部这几条顺序同样是刻意的，调换之前先看清理由：

| 位置 | 中间件 | 为什么在这里 |
|---|---|---|
| 1 | Auth | 必须先确认"你是谁"，后面的限流/配额才有租户可依据；失败 **fail-closed**（拒绝），放行等于开后门 |
| 2 | RateLimit | 在读 body 之前就挡住洪峰，避免垃圾/超大 body 白白走一遍解析。**语义是"按请求次数计"**：被路由拒绝的请求也消耗令牌，因为限流保护的是网关自身的容量 |
| 3 | Routing | 放在配额之前，让"根本没走到后端"的请求（未知模型、body 不是 JSON）**不消耗配额** |
| 4 | Quota | 紧挨转发，采用"**先预扣、转发失败再退款**"：并发下不会超发，同时下游 5xx 不会算到租户的每日额度上 |

配额的计费口径（见 `internal/middleware/quota.go` 的 `shouldRefundQuota`）：

| 最终状态码 | 是否计费 | 说明 |
|---|---|---|
| 2xx / 3xx | 计费 | 正常拿到服务 |
| 4xx（含下游透传的 400/404） | 计费 | 请求确实被处理了，是调用方参数有问题 |
| 5xx（502 下游挂了 / 500 网关内部错误） | **退款** | 租户什么都没拿到，不该为这次调用买单 |

网关自己产生的 400（未知模型、非法 JSON）**根本走不到配额中间件**，由路由中间件提前拦下，也不会计费。

## 本地运行

### 前置依赖

1. **Redis**（限流令牌桶 + 每日配额都存在这里）：

```bash
# 本机没装 Redis 的话，最快的方式是用 Docker 起一个
docker run -d --name gateway-redis -p 6379:6379 redis:7-alpine

# 或者用你已经熟悉的 k3s，写个最简单的 Deployment + Service 也可以，
# 学习阶段用哪种方式都行，关键是让 REDIS_ADDR 指向的实例能连上
# （地址配在 .env 里，不再写在 config.yaml 中）
```

2. **Ollama** 已经跑起来（默认 `http://localhost:11434`），
   并且已经 `ollama pull qwen2.5` 过对应模型。

### 拉取依赖并启动

```bash
cd gateway
go mod tidy             # 拉取 gin / go-redis / pgx / yaml.v3 依赖（沙盒环境需联网）

cp .env.example .env    # 填入真实的 Redis / PostgreSQL 地址和密码
go run .                # 自动加载 .env，读取 config/config.yaml，监听 :8080
```

启动时会按顺序做三件事，任何一步失败都会**直接退出**，不会带着坏配置继续跑：

1. 加载 `.env` 并展开 `config.yaml` 里的 `${VAR}` 占位符（残留无法解析的引用会报错）
2. 严格解析配置（多余/拼错的字段会报错，比如残留的 `tenants:` 段）
3. Ping Redis 和 PostgreSQL（"初始化 Redis 失败"这类报错就是这一步）

这是故意设计成这样的——配置错误要在启动阶段就暴露，而不是留到线上流量
进来才发现连不上。

跑一遍测试（不需要 Redis / PostgreSQL / Ollama）：

```bash
go test ./...
```

## 验收命令清单

### 1. 健康检查（无需鉴权）

```bash
# 存活探针：只看进程自己，不查依赖
curl http://localhost:8080/healthz
# 期待: {"status":"ok"}

# 就绪探针：检查 Redis / PostgreSQL，任一不可用就返回 503
curl -i http://localhost:8080/readyz
# 期待: {"checks":[{"name":"redis","ok":true},{"name":"postgres","ok":true}],"status":"ok"}

# 观察两者差别：停掉 PostgreSQL 之后
docker stop gateway-postgres
curl -s -o /dev/null -w "healthz=%{http_code}\n" http://localhost:8080/healthz  # 200（进程还活着）
curl -s -o /dev/null -w "readyz=%{http_code}\n"  http://localhost:8080/readyz   # 503（不能接流量了）
docker start gateway-postgres
# 关键点：依赖挂了只把实例从负载均衡摘出去（readyz 失败），
# 而不是让 K8s 重启它（healthz 仍然 200）——否则一次 Redis 抖动
# 会演变成所有副本一起重启的雪崩
```

### 1.1 指标：Prometheus 抓取格式

```bash
curl -s http://localhost:8080/metrics | grep '^gateway_'
# 期待看到（节选）：
#   gateway_requests_total{method="POST",status="200"} 12
#   gateway_request_duration_seconds_bucket{method="POST",status="200",le="1"} 3
#   gateway_inflight_requests 1
#   gateway_rate_limited_total{tenant="tenant-a"} 5
#   gateway_quota_rejected_total{tenant="tenant-a"} 1
#   gateway_quota_refunds_total{result="ok"} 2
#   gateway_auth_failures_total{reason="invalid_key"} 7
#   gateway_panics_total 0

# 验证计数真的在动：并发打 10 个请求（tenant-a 配置 RPS:2），再抓一次
for i in $(seq 1 10); do
  curl -s -o /dev/null http://localhost:8080/api/generate \
    -H "Authorization: Bearer tenant-a-key" -d '{"model":"qwen2.5","prompt":"hi"}' &
done
wait
curl -s http://localhost:8080/metrics | grep -E 'gateway_(requests_total|rate_limited_total)'
```

> `/metrics` 和 `/readyz` 都**不要暴露到公网**：前者含租户 ID 和容量水位，
> 后者会返回内部组件的错误详情。放在内网、由 Prometheus/kubelet 访问即可。

### 2. 鉴权：无 key / 错误 key

```bash
curl -i http://localhost:8080/api/generate -d '{"model":"qwen2.5","prompt":"hi"}'
# 期待: 401 missing or malformed authorization header

curl -i http://localhost:8080/api/generate -H "Authorization: Bearer wrong-key" -d '{...}'
# 期待: 401 invalid api key
```

### 3. 流式转发：观察是否逐块到达而不是等全部生成完再一次性返回

```bash
curl -N http://localhost:8080/api/generate \
  -H "Authorization: Bearer tenant-a-key" \
  -d '{"model":"qwen2.5","prompt":"数到10","stream":true}'
```

### 4. 限流：必须用并发请求测试，顺序请求测不出限流效果

```bash
# tenant-a 配置 RPS:2 Burst:2，10 个并发应该是约 2 个 200，其余 429
for i in $(seq 1 10); do
  curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/api/generate \
    -H "Authorization: Bearer tenant-a-key" -d '{"model":"qwen2.5","prompt":"hi"}' &
done
wait
```

```bash
# 验证租户隔离：tenant-b 配置 RPS:5 Burst:5，不受 tenant-a 限流影响
for i in $(seq 1 10); do
  curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/api/generate \
    -H "Authorization: Bearer tenant-b-key" -d '{"model":"qwen2.5","prompt":"hi"}' &
done
wait
```

### 4.1 【Redis 版新增验证】多实例共享限流状态

这是这次从内存版升级到 Redis 版之后，真正新增的能力，值得专门验证一下。
内存版做不到这件事——每个进程各自维护限流状态，这次验证的就是
"换成 Redis 之后，两个进程共享同一份限流额度"。

```bash
# 开两个终端，用不同端口跑两个网关实例，指向同一个 Redis
go run . -config config/config.yaml &   # 实例A，监听 :8080（config.yaml 里配的端口）

# 复制一份配置改成 8081 端口，启动第二个实例
cp config/config.yaml config/config-b.yaml
sed -i 's/port: 8080/port: 8081/' config/config-b.yaml
go run . -config config/config-b.yaml &  # 实例B，监听 :8081

# 现在对两个不同端口的实例交替发起请求，观察它们是否共享同一份限流额度
for i in $(seq 1 10); do
  port=$((8080 + i % 2))  # 交替打向 8080 和 8081
  curl -s -o /dev/null -w "instance:$port -> %{http_code}\n" \
    http://localhost:$port/api/generate \
    -H "Authorization: Bearer tenant-a-key" -d '{"model":"qwen2.5","prompt":"hi"}' &
done
wait
```

期待结果：tenant-a 配置 `Burst:2`，10 个请求不管落在哪个端口，加起来
也应该只有约 2 个 200，其余 429——因为两个实例查的是同一个 Redis key
（`ratelimit:tenant-a`）。如果每个实例各自放行了 2 个（总共 4 个 200），
说明限流状态没有真正共享，需要检查两个实例是不是连到了同一个 Redis 地址。

### 4.2 【Redis 版新增验证】Redis 故障时的降级行为

```bash
# 停掉 Redis，观察网关是否还能正常处理请求（应该能，因为采用了 fail-open 策略）
docker stop gateway-redis

curl -i http://localhost:8080/api/generate -H "Authorization: Bearer tenant-a-key" \
  -d '{"model":"qwen2.5","prompt":"hi"}'
# 期待：请求依然被放行（200），但网关日志里应该打印出
# "[WARN] 限流检查失败，已降级放行" 这类告警信息

docker start gateway-redis  # 测完记得重新启动 Redis
```

### 5. 配额：临时把 tenant-a 的 daily_cap 改成 5 方便测试

> 注意：租户参数已经不在 config.yaml 里（改了会启动报错，这是故意的）。
> 要改 daily_cap，走数据库或种子文件，见下方"阶段二"章节。

```bash
for i in $(seq 1 8); do
  curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/api/generate \
    -H "Authorization: Bearer tenant-a-key" -d '{"model":"qwen2.5","prompt":"hi"}'
  sleep 1   # 间隔拉开，避免被限流拦下，专门测配额
done
# 期待: 前 5 个 200，第 6 个开始 429 daily quota exceeded
```

### 5.1 【新增验证】失败请求不扣配额、未知模型不扣配额

这次改造把"计费点"从"请求到达"挪到了"请求真正拿到服务"，
用下面两组命令可以直接观察到差别：

```bash
# 准备：先把 tenant-a 的今日配额用量记下来
docker exec -i gateway-redis redis-cli GET "quota:tenant-a:$(date +%F)"
# 例： "12"

# 场景 A：未知模型（网关自己拒掉，不该扣配额）
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/api/generate \
  -H "Authorization: Bearer tenant-a-key" -d '{"model":"not-exist","prompt":"hi"}'
# 期待: 400
docker exec -i gateway-redis redis-cli GET "quota:tenant-a:$(date +%F)"
# 期待: 计数不变，仍是 12

# 场景 B：下游 502（预扣了，但请求结束后自动退还）
docker stop <ollama 容器名>     # 让模型后端不可用
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/api/generate \
  -H "Authorization: Bearer tenant-a-key" -d '{"model":"qwen2.5","prompt":"hi"}'
# 期待: 502
docker exec -i gateway-redis redis-cli GET "quota:tenant-a:$(date +%F)"
# 期待: 计数仍然不变（预扣 +1，随后退款 -1，净变化为 0）
# 如果看到计数被 +1 且没有回退，说明退款逻辑没生效
docker start <ollama 容器名>
```

对应的自动化测试（不需要 Redis/PostgreSQL/Ollama）：

```bash
go test ./...
```

### 6. 多模型路由：未知模型应该被拒绝

```bash
curl -i http://localhost:8080/api/generate -H "Authorization: Bearer tenant-a-key" \
  -d '{"model":"not-exist","prompt":"hi"}'
# 期待: 400 unknown model: not-exist
```

### 7. 后端不可用时的降级（把 Ollama 停掉再试）

```bash
curl -i http://localhost:8080/api/generate -H "Authorization: Bearer tenant-a-key" \
  -d '{"model":"qwen2.5","prompt":"hi"}'
# 期待: 502 upstream backend unavailable，而不是卡住不响应
```

## 已完成的三轮加固

### 第一轮：安全与清理

- **配置文件里的明文密码全部外置**：`config/config.yaml` 现在只写 `${VAR}` 占位符，
  真实值在 `.env`（已 gitignore）或环境变量/Secret 里；支持 `${VAR:-默认值}`
- **删掉"配置即数据源"的残留**：`tenants:` 段和 `config.TenantConfig` 已删除，
  配置文件里再出现 `tenants:` 会**启动直接报错**（而不是被静默忽略，让人误以为改 yaml 能改限额）
- **补齐建表脚本** `migrations/0001_init_tenants.sql`（`sui_tenants` / `sui_api_keys`），
  表结构不再只存在于某台机器的数据库里
- **删除死代码**：`internal/pgstore/pool.go` 里复制粘贴的代理池代码、注释掉的旧 `auth.Store`、
  `main.go` 里的旧初始化代码；旧的内存版实现统一存到 `docs/legacy-inmemory-examples.md`
- **`cmd/seed` 增加输入校验**：重复 id / 重复 api_key / 非法 rps 都会提前报错，
  不再出现"导入成功但请求 401"这种难查的现象；且与网关共用同一套 Redis 超时/连接池参数
- 清理 `.DS_Store`，新增 `.gitignore`（含 `.env`、本地配置、构建产物）

### 第二轮：计费语义与中间件顺序

- 中间件顺序调整为 **Auth → RateLimit → Routing → Quota → 转发**，
  让"没走到后端"的请求不再消耗租户配额
- 配额改为**先预扣、转发失败再退款**（`Consume` + `Refund`，退款用 Lua 原子钳位，
  不会把计数退成负数、也不会留下永不过期的 key）
- 计费口径明确为：**2xx/3xx/4xx 计费，5xx 退款**，写成 `shouldRefundQuota` 单点判断
- 退款使用 `context.WithoutCancel`：客户端超时断开也不会导致退款失败
- 所有 `c.Get(...).(*auth.Tenant)` 裸断言改成安全断言，顺序被改错时给出明确错误而不是 panic
- 为了能测试顺序，引擎装配从 `main()` 抽到 `engine.go` 的 `buildEngine()`；
  限流/配额中间件改为依赖接口，测试用假实现即可，无需起 Redis
- `proxy.Pool` 支持注入 `http.RoundTripper`（`proxy.WithTransport`），
  为后续定制上游超时/连接池留好入口

### 第三轮：可观测性（日志 / 探针 / 指标）

- **结构化日志**：`fmt.Printf` 全部换成标准库 `log/slog`，格式/级别可配
  （`LOG_FORMAT=json`、`LOG_LEVEL=debug`）。请求日志自动带
  `request_id`（挂在请求 context 的日志器上，连 pgstore/ratelimit 这类
  不认识 gin 的底层包也会带上），按状态码自动分级（5xx→Error、4xx→Warn）
- **`X-Request-ID` 透传 + 校验**：调用方传了就用它的（上下游能串成一个链路），
  但会校验字符集和长度——否则可以塞超长内容撑爆日志，或塞换行**伪造日志记录**（log injection）
- **存活/就绪探针分离**：`/healthz` 只回答"进程还活着"（不查依赖，避免依赖抖动导致
  K8s 把所有副本一起重启）；`/readyz` 检查 Redis/PostgreSQL，失败返回 503 让 LB 摘流量。
  探针结果缓存 1 秒，防止高频探测把依赖压垮
- **Prometheus 指标 `/metrics`**：请求量与状态码、延迟直方图（按 LLM 场景重新分桶，
  最大 300s）、处理中请求数、限流/配额拒绝次数（按租户）、鉴权失败原因、配额退款结果、
  panic 次数，外加 Go 运行时指标（goroutine/GC/内存）
  - 故意**不打 path 标签**：`/api/*` 路径千变万化，打上去就是标签基数爆炸
  - 故意**不统计 `/metrics` 自己**：否则抓取请求会把 `inflight` 永远抬成 1

## 尚未处理 & 后续可以扩展的方向

- **没有接入分布式追踪**：request_id 能串起网关自己的日志，
  但没有 OpenTelemetry 的 span 树，跨服务（网关 → 模型后端）的调用链还看不到
- **指标缺少告警规则**：`gateway_quota_refunds_total{result="failed"}`、
  `gateway_auth_failures_total` 突增这类信号还没有配套的告警配置
- **流式长响应受 `server.write_timeout_seconds`（默认 300s）限制**，
  超长对话会被掐断；上游 Transport 也没有单独的超时/连接池配置
- **路由能力仍偏弱**：`ModelRoute` 只有 `Backend` 一个字段，
  没有 path 重写/前缀剥离、没有 OpenAI(`/v1/chat/completions`) ↔ Ollama 协议适配、
  没有多后端加权与故障摘除
- **用量数据没有持久化**：无法做用量统计和计费，规划见文末"下一步"
- **测试覆盖还很薄**：目前只有配置加载和环境变量插值、以及引擎顺序/计费语义的测试，
  鉴权缓存路径、令牌桶 Lua、pgstore 的 cache-aside 都还没有覆盖
- **没有 mTLS**：内部服务间通信建议至少走 HTTPS，敏感场景上双向 TLS

## 阶段二：PostgreSQL 接管租户管理

租户数据不再放在 config.yaml 里，改成存在 PostgreSQL，鉴权路径变成
"先查 Redis 缓存 -> 未命中查 PostgreSQL -> 查到后写回缓存"。

### 新增文件说明

```
migrations/0001_init_tenants.sql   # 建表脚本：sui_tenants + sui_api_keys 两张表
internal/pgstore/pool.go            # PostgreSQL 连接池初始化
internal/pgstore/tenant_store.go    # 核心：cache-aside 查找 + 租户/Key 管理操作
seed/tenants.yaml                   # 种子数据：要导入数据库的初始租户（明文 key）
cmd/seed/main.go                    # 独立命令行工具：把种子数据写入数据库
```

### 准备工作：启动 PostgreSQL 并建表

```bash
# 用 Docker 起一个本地 PostgreSQL（跟之前起 Redis 的方式一样）
docker run -d --name gateway-postgres \
  -e POSTGRES_USER=gateway \
  -e POSTGRES_PASSWORD=gateway_dev_password \
  -e POSTGRES_DB=gateway_db \
  -p 5432:5432 \
  postgres:16-alpine

# 等待几秒让数据库完全启动后执行建表脚本
psql "postgres://gateway:gateway_dev_password@localhost:5432/gateway_db?sslmode=disable" \
  -f migrations/0001_init_tenants.sql
```

如果本机没有 `psql` 命令，也可以用 Docker 里自带的客户端执行：

```bash
docker exec -i gateway-postgres psql -U gateway -d gateway_db < migrations/0001_init_tenants.sql
```

### 导入种子租户数据

```bash
go mod tidy   # 补上新增的 pgx 依赖
go run ./cmd/seed -config config/config.yaml -seed seed/tenants.yaml
```

期待输出：

```
✓ 已导入租户 tenant-a（订单系统A）rps=2 burst=2 daily_cap=1000
✓ 已导入租户 tenant-b（订单系统B）rps=5 burst=5 daily_cap=5000
种子数据导入完成，共 2 个租户（耗时 8ms）
```

导入前会先校验种子文件：重复 id、重复 api_key、非法的 rps/burst/daily_cap
都会直接报错退出，不会写一半进数据库。

### 启动网关

```bash
go run .
```

### 验收命令

**1. 基本鉴权仍然正常（数据源换了，行为应该不变）**

```bash
curl -i http://localhost:8080/api/generate -H "Authorization: Bearer tenant-a-key" \
  -d '{"model":"qwen2.5","prompt":"hi"}'
# 期待：跟之前一样正常透传，说明数据库查找路径工作正常
```

**2. 验证缓存生效：直接改数据库，观察多长时间后生效**

```bash
# 直接改 tenant-a 的 rps，不通过种子工具（模拟"没有主动清缓存"的场景）
docker exec -i gateway-postgres psql -U gateway -d gateway_db \
  -c "UPDATE sui_tenants SET rps = 100 WHERE id = 'tenant-a';"

# 立刻测试限流：因为 Redis 里还缓存着旧值（rps=2），这次请求应该还是按
# 旧参数限流，改动不会立刻生效
for i in $(seq 1 10); do
  curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/api/generate \
    -H "Authorization: Bearer tenant-a-key" -d '{"model":"qwen2.5","prompt":"hi"}' &
done
wait

# 等 5 分钟缓存过期后（或者手动 FLUSHDB 清空 Redis 加速测试），
# 再跑一次，这时应该已经用上新的 rps=100，不容易再触发 429
```

这个实验能直观感受到"缓存 TTL 造成的最终一致性延迟"是真实存在的，
不是理论上的东西——这也是为什么 `RevokeAPIKey` 这类安全相关的操作
必须主动清缓存，不能依赖 TTL 自然过期。

**3. 验证吊销 Key 立刻生效**

吊销操作目前只在 `pgstore.TenantStore.RevokeAPIKey` 里实现了，
学习阶段可以写个一次性小程序调用它，或者直接在 psql 里操作 + 手动清缓存：

```bash
# 数据库标记吊销
docker exec -i gateway-postgres psql -U gateway -d gateway_db -c "
  UPDATE sui_api_keys SET revoked_at = now()
  WHERE key_hash = encode(sha256('tenant-a-key'::bytea), 'hex');
"

# 主动清掉 Redis 缓存（模拟 RevokeAPIKey 内部做的事）
docker exec -i gateway-redis redis-cli DEL "tenant:$(echo -n 'tenant-a-key' | sha256sum | cut -d' ' -f1)"

# 立刻测试，应该马上就是 401，不需要等缓存过期
curl -i http://localhost:8080/api/generate -H "Authorization: Bearer tenant-a-key" \
  -d '{"model":"qwen2.5","prompt":"hi"}'
# 期待：401 invalid api key
```

**4. 验证 fail-closed：PostgreSQL 挂了，鉴权应该拒绝而不是放行**

```bash
# 先清空 Redis 缓存，确保这次请求一定会打到数据库（而不是命中缓存跳过数据库）
docker exec -i gateway-redis redis-cli FLUSHDB

# 停掉 PostgreSQL
docker stop gateway-postgres

# 用一个全新的、从未出现过的 key 测试（避免命中负向缓存）
curl -i http://localhost:8080/api/generate -H "Authorization: Bearer never-seen-key" \
  -d '{"model":"qwen2.5","prompt":"hi"}'
# 期待：503 auth service temporarily unavailable
# 而不是放行（200）或者笼统地报 401——503 说明网关识别出
# "这是我这边的问题，不是你的 key 有问题"

docker start gateway-postgres  # 测完记得重新启动
```

## 下一步：用量审计日志（规划中）

计划新增 `usage_logs` 表，记录每次调用的租户、模型、状态码、耗时，
写入方式采用异步批量（channel + 后台 goroutine），避免拖慢主请求路径。
