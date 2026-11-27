package sub

import "github.com/metacubex/mihomo/adapter"

// ValidateWithMihomo 用与运行时相同的解析逻辑校验节点字段（锁定 Mihomo 版本见 go.mod）。
// 只构造出站对象，不建立连接。
func ValidateWithMihomo(m map[string]any) error {
	p, err := adapter.ParseProxy(cloneMap(m))
	if err != nil {
		return err
	}
	return p.Close()
}
