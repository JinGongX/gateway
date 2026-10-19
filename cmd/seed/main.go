// cmd/seed 是一个独立的命令行工具，负责把 seed/tenants.yaml 里定义的
// 租户和 API Key 写入 PostgreSQL。
//
// 学习点：这是企业级项目里常见的"运维/管理类脚本单独放在 cmd/ 目录下"
// 的组织方式。它跟主程序 main.go 共享同一套内部包（pgstore、config），
// 但编译出来是完全独立的可执行文件，不会被打包进网关服务本身。
//
// 密钥边界：明文 API Key 只存在于这个工具的输入文件（seed/tenants.yaml）
// 和 .env 里，网关进程的配置文件中不含任何明文 key。
//
// 用法：
//
//	go run ./cmd/seed -config config/config.yaml -seed seed/tenants.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"gateway/internal/config"
	"gateway/internal/pgstore"
	"gateway/internal/redisclient"

	"gopkg.in/yaml.v3"
)

// tenantSeed 对应 seed/tenants.yaml 里的一项。
//
// 这个结构体定义在 seed 工具自己这里，而不是复用 config 包的类型，
// 原因是它属于"导入用的输入格式"，跟网关运行时的配置没有关系；
// 放在一起会让 config 包看起来像是"连租户都要从 yaml 读"。
type tenantSeed struct {
	ID       string  `yaml:"id"`
	Name     string  `yaml:"name"`
	APIKey   string  `yaml:"api_key"`
	RPS      float64 `yaml:"rps"`
	Burst    int     `yaml:"burst"`
	DailyCap int     `yaml:"daily_cap"`
}

type seedFile struct {
	Tenants []tenantSeed `yaml:"tenants"`
}

// validate 在写库之前先把种子文件自身的问题挑出来。
// 学习点：seed 是幂等脚本，经常被反复执行，如果不校验唯一性，
// 重复的 api_key 会让后面的租户静默地"注册不上 key"（INSERT ... ON CONFLICT DO NOTHING），
// 表现是"导入成功但请求 401"，这种问题排查成本很高，不如提前报错。
func (s seedFile) validate() error {
	if len(s.Tenants) == 0 {
		return fmt.Errorf("种子数据文件里没有任何租户")
	}

	seenIDs := make(map[string]bool, len(s.Tenants))
	seenKeys := make(map[string]bool, len(s.Tenants))
	for i, t := range s.Tenants {
		if t.ID == "" {
			return fmt.Errorf("第 %d 个租户缺少 id", i+1)
		}
		if t.APIKey == "" {
			return fmt.Errorf("租户 %s 缺少 api_key", t.ID)
		}
		if seenIDs[t.ID] {
			return fmt.Errorf("发现重复的租户 id: %s", t.ID)
		}
		if seenKeys[t.APIKey] {
			return fmt.Errorf("发现重复的 api_key（归属于租户 %s）", t.ID)
		}
		seenIDs[t.ID] = true
		seenKeys[t.APIKey] = true

		if t.RPS <= 0 {
			return fmt.Errorf("租户 %s 的 rps 必须大于 0", t.ID)
		}
		if t.Burst <= 0 {
			return fmt.Errorf("租户 %s 的 burst 必须大于 0", t.ID)
		}
		if t.DailyCap < 0 {
			return fmt.Errorf("租户 %s 的 daily_cap 不能为负数", t.ID)
		}
	}
	return nil
}

func main() {
	configPath := flag.String("config", "config/config.yaml", "网关运行时配置文件路径（用来拿数据库连接信息）")
	seedPath := flag.String("seed", "seed/tenants.yaml", "种子数据文件路径")
	flag.Parse()

	// 跟 main.go 走同一条配置加载路径：先加载 .env，再展开 ${VAR} 占位符，
	// 所以数据库密码同样不需要出现在配置文件里。
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	seedData, err := os.ReadFile(*seedPath)
	if err != nil {
		log.Fatalf("读取种子数据文件失败: %v", err)
	}
	var seed seedFile
	if err := yaml.Unmarshal(seedData, &seed); err != nil {
		log.Fatalf("解析种子数据文件失败: %v", err)
	}
	if err := seed.validate(); err != nil {
		log.Fatalf("种子数据校验失败: %v", err)
	}

	ctx := context.Background()
	start := time.Now()

	pgPool, err := pgstore.NewPool(cfg.Postgres.DSN())
	if err != nil {
		log.Fatalf("连接 PostgreSQL 失败: %v", err)
	}
	defer pgPool.Close()

	// 种子工具也需要连 Redis，原因：写入新租户或者更新已有租户信息之后，
	// 如果这个租户之前已经被缓存过（比如你在测试环境反复调整某个租户的
	// rps 参数），如果不清掉旧缓存，网关会继续使用缓存里的旧值，
	// 最长要等 5 分钟缓存过期才能生效，种子工具应该主动清掉这层缓存。
	redisClient, err := redisclient.FromSettings(
		cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB,
		cfg.Redis.DialTimeoutMs, cfg.Redis.ReadTimeoutMs, cfg.Redis.WriteTimeoutMs,
		cfg.Redis.PoolSize,
	)
	if err != nil {
		log.Fatalf("连接 Redis 失败: %v", err)
	}
	defer redisClient.Close()

	store := pgstore.NewTenantStore(pgPool, redisClient)

	for _, t := range seed.Tenants {
		if err := store.CreateTenant(ctx, pgstore.CreateTenantInput{
			ID: t.ID, Name: t.Name, RPS: t.RPS, Burst: t.Burst, DailyCap: t.DailyCap,
		}); err != nil {
			log.Fatalf("创建租户 %s 失败: %v", t.ID, err)
		}

		if err := store.RegisterAPIKey(ctx, t.ID, t.APIKey, "seed-import"); err != nil {
			log.Fatalf("为租户 %s 注册 API Key 失败: %v", t.ID, err)
		}

		// 主动清缓存，保证这次导入的最新值立刻生效，不用等旧缓存过期
		if err := store.InvalidateCache(ctx, t.APIKey); err != nil {
			fmt.Printf("[WARN] 清除租户 %s 缓存失败（不影响导入结果）: %v\n", t.ID, err)
		}

		fmt.Printf("✓ 已导入租户 %s（%s）rps=%g burst=%d daily_cap=%d\n", t.ID, t.Name, t.RPS, t.Burst, t.DailyCap)
	}

	fmt.Printf("种子数据导入完成，共 %d 个租户（耗时 %s）\n", len(seed.Tenants), time.Since(start).Round(time.Millisecond))
}
