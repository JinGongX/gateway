package config

import (
	"fmt"
	"sort"
)

// validateNonNegative 检查一组「超时 / 数量」类配置都不是负数。
//
// 0 是合法值：在这些字段里它表示"使用默认值"或"关闭"（具体含义见各字段注释），
// 但负数在任何字段里都只可能是笔误（比如 -1 被某些库当成"无限"，而这里并不是），
// 与其带着含义不明的值启动，不如直接报错。
// 按字段名排序后检查，保证同一份配置每次报的都是同一个错误，方便排查。
func validateNonNegative(values map[string]int) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if values[name] < 0 {
			return fmt.Errorf("%s 不能为负数（当前 %d）", name, values[name])
		}
	}
	return nil
}
