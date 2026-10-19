// Package config 负责加载和解析 config.yaml。
//
// 学习点：企业级项目里，"配置加载"通常单独成一个包，跟业务逻辑解耦。
// main.go 只管调用 config.Load()，不关心 yaml 怎么解析的细节。
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gateway/internal/logger"

	"gopkg.in/yaml.v3"
)

// ServerConfig 对应 yaml 里的 server 节点
type ServerConfig struct {
	Port                int   `yaml:"port"`
	ReadTimeoutSeconds  int   `yaml:"read_timeout_seconds"`
	WriteTimeoutSeconds int   `yaml:"write_timeout_seconds"`
	MaxBodyBytes        int64 `yaml:"max_body_bytes"`
}

// RedisConfig 对应 yaml 里的 redis 节点
type RedisConfig struct {
	Addr           string `yaml:"addr"`
	Password       string `yaml:"password"`
	DB             int    `yaml:"db"`
	DialTimeoutMs  int    `yaml:"dial_timeout_ms"`
	ReadTimeoutMs  int    `yaml:"read_timeout_ms"`
	WriteTimeoutMs int    `yaml:"write_timeout_ms"`
	PoolSize       int    `yaml:"pool_size"`
}

// PostgresConfig 对应 yaml 里的 postgres 节点
type PostgresConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"` // 本地开发用 disable，生产环境应该用 require 或更高
}

// DSN 拼出 pgx 连接池能识别的连接字符串。
// 学习点：单独写一个方法拼接连接串，而不是让调用方自己拼 fmt.Sprintf，
// 是为了把"配置字段怎么组合成 DSN"这个格式细节封装在 config 包内部，
// main.go 只需要调用 cfg.Postgres.DSN()，不需要知道 DSN 的具体格式。
//
//	func (p PostgresConfig) DSN() string {
//		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
//			p.User, p.Password, p.Host, p.Port, p.DBName, p.SSLMode)
//	}
func (p PostgresConfig) DSN() string {
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(p.User, p.Password),
		Host:     fmt.Sprintf("%s:%d", p.Host, p.Port),
		Path:     "/" + p.DBName,
		RawQuery: "sslmode=" + url.QueryEscape(p.SSLMode),
	}
	return u.String()
}

// LogConfig 对应 yaml 里的 log 节点。
//
// 这里用类型别名而不是重新定义一个同构的结构体：日志级别和格式的
// 合法取值范围只有 logger 包知道（它负责校验和报错），两处各定义一份
// 迟早会出现"配置文件里加了字段但日志包不认"的错位。
// 写成别名，配置解析出来就能直接交给 logger.New 用。
type LogConfig = logger.Config

// ModelConfig 对应 yaml 里 models 列表的每一项
type ModelConfig struct {
	Name    string `yaml:"name"`
	Backend string `yaml:"backend"`
}

// Config 是整个配置文件解析后的顶层结构。
// 注意这里没有 tenants 字段：租户主数据在 PostgreSQL 里（见 internal/pgstore），
// 运行时配置只负责"连到哪个基础设施"和"模型路由表"。
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Log      LogConfig      `yaml:"log"`
	Redis    RedisConfig    `yaml:"redis"`
	Postgres PostgresConfig `yaml:"postgres"`
	Models   []ModelConfig  `yaml:"models"`
}

// Load 从指定路径读取并解析 yaml 配置文件。
// 用法：cfg, err := config.Load("config/config.yaml")
//
// 加载流程：先加载 .env（如果存在）-> 读 yaml -> 展开 ${VAR} 插值 -> 校验。
// 之所以先加载 .env 再展开，是为了让"密码写在 .env、yaml 里只写占位符"
// 这套做法能直接跑起来，yaml 文件本身可以安全地进版本库。
func Load(path string) (*Config, error) {
	if err := LoadDotEnv(EnvFilePath()); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	expanded := expandEnvRefs(string(data))
	// 兜底检查：如果还残留 "${"，说明写了一个本包不认识的引用语法
	// （比如把 ${VAR:-x} 写成了 ${VAR-x}），与其带着一个奇怪的值继续跑，
	// 不如在启动阶段就明确报错。
	if strings.Contains(expanded, "${") {
		return nil, fmt.Errorf("配置文件 %s 里存在无法解析的变量引用，请检查是否写成 ${VAR} 或 ${VAR:-默认值}", path)
	}

	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(expanded))
	// KnownFields(true)：yaml 里出现结构体上没有的字段时直接报错。
	//
	// 学习点：yaml.Unmarshal 的默认行为是"忽略不认识的字段"，这在配置
	// 迁移期间特别危险 —— 租户配置已经搬到 PostgreSQL 了，如果还有人
	// 在 config.yaml 里改 daily_cap，改动不生效而且没有任何提示，
	// 最后会变成"我明明配了限流参数，怎么不生效"这类很难查的问题。
	// 宁可启动失败，也不要静默忽略。
	decoder.KnownFields(true)

	if err := decoder.Decode(&cfg); err != nil {
		if strings.Contains(err.Error(), "tenants") {
			return nil, fmt.Errorf("解析配置文件失败: %w"+
				"（提示：租户配置已迁移到 PostgreSQL，不再从 config.yaml 读取，"+
				"请改用 cmd/seed 导入或直接修改数据库）", err)
		}
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("配置校验失败: %w", err)
	}

	return &cfg, nil
}

// validate 做基本的配置合法性检查。
// 学习点：企业级代码里，加载配置后一定要校验，而不是假设配置文件永远写对。
// 提前失败（fail fast）比运行到一半才发现配置错误要好得多。
func (c *Config) validate() error {
	if c.Server.Port == 0 {
		return fmt.Errorf("server.port 不能为空")
	}
	if c.Redis.Addr == "" {
		return fmt.Errorf("redis.addr 不能为空")
	}
	if c.Postgres.Host == "" || c.Postgres.DBName == "" {
		return fmt.Errorf("postgres.host 和 postgres.dbname 不能为空")
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("至少需要配置一个模型路由")
	}
	return nil
}
