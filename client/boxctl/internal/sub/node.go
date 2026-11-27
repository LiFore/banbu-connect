package sub

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// secretFields 是需要加密存储的凭据字段（M1-08）；同时作为稳定 ID 的“关键凭据”。
// 嵌套字段用 "父.子" 表示。
var secretFields = []string{
	"password", "uuid", "username", "private-key", "pre-shared-key",
	"auth-str", "auth", "token", "psk", "obfs-password",
	"plugin-opts.password",
}

// SplitSecrets 把代理配置拆成公开部分与凭据部分，原 map 不变。
func SplitSecrets(m map[string]any) (public, secrets map[string]any) {
	public = cloneMap(m)
	secrets = map[string]any{}
	for _, f := range secretFields {
		parent, child, nested := strings.Cut(f, ".")
		if !nested {
			if v, ok := public[f]; ok {
				secrets[f] = v
				delete(public, f)
			}
			continue
		}
		pm, ok := public[parent].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := pm[child]; ok {
			pm = cloneMap(pm)
			secrets[f] = v
			delete(pm, child)
			public[parent] = pm
		}
	}
	return public, secrets
}

// MergeSecrets 是 SplitSecrets 的逆操作。
func MergeSecrets(public, secrets map[string]any) map[string]any {
	m := cloneMap(public)
	for f, v := range secrets {
		parent, child, nested := strings.Cut(f, ".")
		if !nested {
			m[f] = v
			continue
		}
		pm, _ := m[parent].(map[string]any)
		pm = cloneMap(pm)
		pm[child] = v
		m[parent] = pm
	}
	return m
}

// NodeID 计算稳定节点 ID：hash(协议, server, port, 关键凭据)，与节点名无关（M2-08）。
// 名称之外完全相同的节点得到相同 ID，即视为重复。
func NodeID(m map[string]any) string {
	_, secrets := SplitSecrets(m)
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	fmt.Fprintf(h, "%v\x00%v\x00%d", m["type"], m["server"], PortOf(m))
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%s=%v", k, secrets[k])
	}
	return "n_" + hex.EncodeToString(h.Sum(nil))[:16]
}

// PortOf 兼容 YAML(int)、JSON(float64) 与字符串端口。
func PortOf(m map[string]any) int {
	switch v := m["port"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		p, _ := strconv.Atoi(v)
		return p
	}
	return 0
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
