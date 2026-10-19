package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile 把内容写进临时目录，返回文件路径。
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	return path
}

// testYAML 是一份最小可运行配置，两个 %s 分别是 redis.addr 和 redis.password。
// 这两个位置正是需要验证插值行为的地方。
func testYAML(addr, password string) string {
	return fmt.Sprintf(`
server:
  port: 8080
redis:
  addr: %s
  password: %s
postgres:
  host: db
  port: 5432
  user: u
  password: p
  dbname: d
  sslmode: disable
models:
  - name: m
    backend: http://localhost:1
`, addr, password)
}

func TestLoadExpandsEnvRefs(t *testing.T) {
	// 已设置的环境变量优先；未设置的走 ${VAR:-默认值}
	t.Setenv("GW_TEST_REDIS_ADDR", "10.0.0.1:6380")
	os.Unsetenv("GW_TEST_REDIS_PASS")

	path := writeFile(t, "config.yaml", testYAML("${GW_TEST_REDIS_ADDR:-localhost:6379}", "${GW_TEST_REDIS_PASS:-}"))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.Redis.Addr != "10.0.0.1:6380" {
		t.Errorf("环境变量应优先生效，得到 addr=%q", cfg.Redis.Addr)
	}
	if cfg.Redis.Password != "" {
		t.Errorf("未设置的环境变量应取默认值（空串），得到 password=%q", cfg.Redis.Password)
	}
}

func TestLoadUsesDefaultWhenEnvMissing(t *testing.T) {
	os.Unsetenv("GW_TEST_ABSENT")
	path := writeFile(t, "config.yaml", testYAML("${GW_TEST_ABSENT:-localhost:6379}", ""))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.Redis.Addr != "localhost:6379" {
		t.Errorf("期望默认值 localhost:6379，得到 %q", cfg.Redis.Addr)
	}
}

func TestLoadRejectsMalformedReference(t *testing.T) {
	// ${VAR-x} 不是本包支持的语法，必须启动即报错，
	// 而不是带着一个奇怪的值继续跑（漏掉一个连字符就是一次线上事故）
	path := writeFile(t, "config.yaml", testYAML("${GW_TEST_BAD-x}", ""))

	_, err := Load(path)
	if err == nil {
		t.Fatal("期望报错，实际通过了")
	}
	if !strings.Contains(err.Error(), "无法解析的变量引用") {
		t.Errorf("期望提示变量引用无法解析，实际错误是: %v", err)
	}
}

func TestLoadRejectsTenantSection(t *testing.T) {
	// 租户数据已经搬到 PostgreSQL，配置文件里再出现 tenants 说明用的是旧版配置，
	// 这种时候静默忽略比报错更危险：用户会以为改 yaml 能改租户限额。
	path := writeFile(t, "config.yaml", testYAML("localhost:6379", "")+`
tenants:
  - id: t1
    api_key: k1
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("旧版 tenants 配置应该被明确拒绝，实际加载成功了")
	}
	if !strings.Contains(err.Error(), "tenants") {
		t.Errorf("报错信息里应指出 tenants 已废弃，实际: %v", err)
	}
}

func TestLoadDotEnvKeepsExistingEnv(t *testing.T) {
	envPath := writeFile(t, ".env", "GW_TEST_FROM_FILE=file-value\nGW_TEST_EXISTING_ENV=file-value\n")
	t.Setenv("GW_TEST_EXISTING_ENV", "env-value")
	os.Unsetenv("GW_TEST_FROM_FILE")

	if err := LoadDotEnv(envPath); err != nil {
		t.Fatalf("LoadDotEnv 失败: %v", err)
	}
	if got := os.Getenv("GW_TEST_FROM_FILE"); got != "file-value" {
		t.Errorf(".env 里的新变量应被加载，得到 %q", got)
	}
	// 关键安全属性：环境变量（生产环境由 Secret 注入）优先于 .env 文件
	if got := os.Getenv("GW_TEST_EXISTING_ENV"); got != "env-value" {
		t.Errorf("已存在的环境变量不应被 .env 覆盖，得到 %q", got)
	}
}

func TestLoadDotEnvMissingFileIsNotAnError(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "does-not-exist.env")); err != nil {
		t.Errorf("文件不存在不应该报错（生产环境没有 .env），实际: %v", err)
	}
}

func TestPostgresDSNEscapesCredentials(t *testing.T) {
	p := PostgresConfig{
		Host: "db.internal", Port: 6432, User: "user@corp", Password: "p@ss:w/rd", DBName: "gw", SSLMode: "require",
	}
	got := p.DSN()
	want := "postgres://user%40corp:p%40ss%3Aw%2Frd@db.internal:6432/gw?sslmode=require"
	if got != want {
		t.Errorf("DSN 转义不正确\n got=%s\nwant=%s", got, want)
	}
}
