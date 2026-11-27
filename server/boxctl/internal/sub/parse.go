// Package sub 是订阅引擎的纯逻辑部分：拉取、解析、清洗、地区识别、稳定 ID（M2）。
// 不访问数据库；持久化与差异计算见 internal/subscriptions。
package sub

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/metacubex/mihomo/common/convert"
	"go.yaml.in/yaml/v3"
)

type Format string

const (
	FormatClash Format = "clash" // Clash/Mihomo YAML（含 JSON）的 proxies 列表
	FormatURI   Format = "uri"   // Base64 或明文的分享链接列表
)

var ErrUnknownFormat = errors.New("未识别的订阅格式（既不是 Clash/Mihomo YAML，也不是分享链接列表）")

// Parse 解析订阅内容为 Mihomo 代理配置 map 列表（M2-03）。
func Parse(content []byte) ([]map[string]any, Format, error) {
	content = bytes.TrimPrefix(bytes.TrimSpace(content), []byte("\xef\xbb\xbf"))
	if len(content) == 0 {
		return nil, "", errors.New("订阅内容为空")
	}

	var doc any
	if yaml.Unmarshal(content, &doc) == nil {
		var list []any
		switch v := doc.(type) {
		case map[string]any:
			list, _ = v["proxies"].([]any)
		case []any: // 直接是代理数组（如 Sub-Store 的 JSON 输出）
			list = v
		}
		if len(list) > 0 {
			proxies, err := toProxyMaps(list)
			return proxies, FormatClash, err
		}
	}

	proxies, err := convert.ConvertsV2Ray(content)
	if err != nil {
		return nil, "", fmt.Errorf("解析分享链接：%w", err)
	}
	if len(proxies) == 0 {
		return nil, "", ErrUnknownFormat
	}
	return proxies, FormatURI, nil
}

func toProxyMaps(list []any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("proxies[%d] 不是对象", i)
		}
		out = append(out, m)
	}
	return out, nil
}
