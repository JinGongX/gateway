package config

import (
	"strings"
	"testing"
)

// withTrustedProxies 在 testYAML 的 server 节点下插入 trusted_proxies。
func withTrustedProxies(list string) string {
	return strings.Replace(
		testYAML("localhost:6379", ""),
		"server:\n  port: 8080\n",
		"server:\n  port: 8080\n  trusted_proxies: "+list+"\n",
		1,
	)
}

func TestLoadAcceptsTrustedProxies(t *testing.T) {
	path := writeFile(t, "config.yaml", withTrustedProxies(`["10.0.0.0/8", "192.168.1.1", "fd00::/8"]`))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got := len(cfg.Server.TrustedProxies); got != 3 {
		t.Errorf("应解析出 3 个可信代理，实际 %d", got)
	}
}

func TestLoadDefaultsToNoTrustedProxies(t *testing.T) {
	path := writeFile(t, "config.yaml", testYAML("localhost:6379", ""))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(cfg.Server.TrustedProxies) != 0 {
		t.Errorf("不配置时应不信任任何代理，实际 %v", cfg.Server.TrustedProxies)
	}
}

func TestLoadRejectsInvalidTrustedProxy(t *testing.T) {
	for _, bad := range []string{
		`["10.0.0.0/33"]`,     // 掩码越界
		`["not-an-ip"]`,       // 根本不是 IP
		`["10.0.0.1", "x/8"]`, // 其中一项非法
		`[""]`,                // 空串
	} {
		path := writeFile(t, "config.yaml", withTrustedProxies(bad))
		_, err := Load(path)
		if err == nil {
			t.Errorf("%s 应被拒绝", bad)
			continue
		}
		if !strings.Contains(err.Error(), "trusted_proxies") {
			t.Errorf("%s：报错信息应指出是 trusted_proxies，实际: %v", bad, err)
		}
	}
}
