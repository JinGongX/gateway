// 本文件解决一个问题：配置文件里不能再出现明文密码。
//
// 做法是给 yaml 加上"环境变量插值"能力：
//
//	redis:
//	  addr: ${REDIS_ADDR:-localhost:6379}
//	  password: ${REDIS_PASSWORD:-}
//
// 展开规则：
//  1. ${VAR}         —— 取环境变量，没有就展开成空字符串
//  2. ${VAR:-默认值} —— 取环境变量，没有就用默认值
//
// 变量来源有两处，优先级从高到低：
//  1. 进程环境变量（k8s Secret / docker -e / 手工 export）
//  2. .env 文件（本地开发用，不进版本库，见仓库根目录 .gitignore）
package config

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// EnvFileVarName 用来指定 .env 文件的位置，方便容器环境下换路径。
// 例如 GATEWAY_ENV_FILE=/etc/gateway/env go run main.go
const EnvFileVarName = "GATEWAY_ENV_FILE"

// defaultEnvFile 是默认的 dotenv 文件名，相对进程当前工作目录。
const defaultEnvFile = ".env"

// envRefPattern 匹配 ${VAR} 和 ${VAR:-默认值}。
// 标准库的 os.Expand 只认 ${VAR}，不支持"带默认值"的写法，
// 而配置文件里恰恰需要"没配就用本地默认值"这种能力，所以自己实现。
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// LoadDotEnv 读取 dotenv 文件，把里面的键值对塞进进程环境变量。
//
// 两个刻意的设计：
//   - 文件不存在不报错：生产环境通常没有 .env，密钥由 Secret 直接注入环境变量；
//   - 已存在的环境变量优先：这样 k8s 里的 Secret 一定能覆盖掉本地文件，
//     不会出现"容器里改了 Secret 但被镜像里的 .env 顶掉"这种事故。
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取环境变量文件 %s 失败: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		// 跳过空行和注释行
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf("%s 第 %d 行格式错误，应为 KEY=VALUE", path, lineNo)
		}
		key = strings.TrimSpace(key)
		// 去掉常见的包裹引号：KEY="value" 和 KEY='value'
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" {
			return fmt.Errorf("%s 第 %d 行缺少变量名", path, lineNo)
		}

		if _, exists := os.LookupEnv(key); exists {
			continue // 环境变量优先，不覆盖
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("设置环境变量 %s 失败: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取环境变量文件 %s 失败: %w", path, err)
	}
	return nil
}

// EnvFilePath 返回要加载的 dotenv 文件路径。
func EnvFilePath() string {
	if p := os.Getenv(EnvFileVarName); p != "" {
		return p
	}
	return defaultEnvFile
}

// expandEnvRefs 把文本里的 ${VAR} / ${VAR:-默认值} 替换成实际值。
func expandEnvRefs(text string) string {
	return envRefPattern.ReplaceAllStringFunc(text, func(ref string) string {
		// 正则一定匹配，这里无需判断 ok
		m := envRefPattern.FindStringSubmatch(ref)
		name, fallback := m[1], m[3]
		if v, exists := os.LookupEnv(name); exists {
			return v
		}
		return fallback // ${VAR} 没配且无默认值时，fallback 是空字符串
	})
}
