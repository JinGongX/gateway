package config

import (
	"fmt"
	"net"
	"strings"
)

// validateTrustedProxies 检查 server.trusted_proxies 里的每一项是不是合法的 IP 或 CIDR。
//
// 为什么要在启动阶段校验：它最终会交给 gin 的 SetTrustedProxies。
// 写错一项（比如 10.0.0.0/33）如果拖到 buildEngine 才发现，就是启动期 panic；
// 在配置校验里提前拦下，报错信息能直接指出是第几项、写了什么。
//
// 为什么这个配置值得认真对待：gin 默认"信任所有代理"，也就是任何客户端
// 带一个 X-Forwarded-For: 1.2.3.4 就能改写 c.ClientIP()，进而污染访问日志、
// 以及未来任何基于 IP 的风控/限流。网关默认应当"谁都不信"，
// 只有明确部署在 LB / Ingress 之后时，才把它们的网段列在这里。
func validateTrustedProxies(list []string) error {
	for i, p := range list {
		if strings.Contains(p, "/") {
			if _, _, err := net.ParseCIDR(p); err != nil {
				return fmt.Errorf("server.trusted_proxies[%d] = %q 不是合法的 CIDR: %w", i, p, err)
			}
			continue
		}
		if net.ParseIP(p) == nil {
			return fmt.Errorf("server.trusted_proxies[%d] = %q 不是合法的 IP 或 CIDR", i, p)
		}
	}
	return nil
}
