// Package objstore 是管理服务保存大文件（客户端更新包、云盘文件）的对象存储：
// 阿里云 OSS（私有桶 + 签名地址），或者没有配置 OSS 时的本机目录。
//
// 两种存储对外一样：签名下载地址、浏览器分片直传（签名的分片上传地址）。本机存储的签名地址指向管理服务自己
// （/api/v1/blob…），由 Local.Routes 处理，用进程内的随机密钥签名，重启后失效。
package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// 存储类型，记在每条记录上：切换配置后，旧记录仍从原来的存储读取。
const (
	KindLocal = "local"
	KindOSS   = "oss"
)

// ErrNotFound 表示对象不存在。
var ErrNotFound = errors.New("文件不存在")

// Part 是分片上传完成的一片。
type Part struct {
	Number int    `json:"number"`
	ETag   string `json:"etag"`
}

// SignedURL 是签名地址，Headers 是请求时必须带上的头（OSS 签名时可能包含）。
type SignedURL struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Store 是对象存储。key 是对象路径（不以 / 开头）。
type Store interface {
	Kind() string
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Stat(ctx context.Context, key string) (int64, error)
	Delete(ctx context.Context, key string) error
	// SignGet 返回限时下载地址；fileName 非空时下载按这个文件名保存。
	SignGet(ctx context.Context, key, fileName string, ttl time.Duration) (SignedURL, error)
	CreateUpload(ctx context.Context, key string) (string, error)
	SignPart(ctx context.Context, key, uploadID string, number int, ttl time.Duration) (SignedURL, error)
	CompleteUpload(ctx context.Context, key, uploadID string, parts []Part) error
	AbortUpload(ctx context.Context, key, uploadID string) error
}

// 分片大小：至少 16 MB，片数不超过 1000（OSS 上限 10000 片、每片最大 5 GB）。
const (
	minPartSize = 16 << 20
	maxParts    = 1000
)

// PartSize 按文件大小选分片大小（整 MB），返回分片大小和片数。
func PartSize(size int64) (int64, int) {
	part := int64(minPartSize)
	if need := (size + maxParts - 1) / maxParts; need > part {
		part = (need + (1 << 20) - 1) &^ ((1 << 20) - 1)
	}
	n := int((size + part - 1) / part)
	if n == 0 {
		n = 1
	}
	return part, n
}

// ContentDisposition 生成下载时的文件名头：ASCII 兜底名加 RFC 5987 的 UTF-8 文件名。
func ContentDisposition(fileName string) string {
	fallback := strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII || r < 0x20 || r == '"' || r == '\\' || r == ';' {
			return '_'
		}
		return r
	}, fileName)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, fallback, url.PathEscape(fileName))
}

// CleanKey 去掉 key 里的 .. 和多余的 /，防止本机存储越出根目录。
func CleanKey(key string) (string, error) {
	parts := []string{}
	for _, p := range strings.Split(strings.ReplaceAll(key, "\\", "/"), "/") {
		switch p {
		case "", ".":
		case "..":
			return "", fmt.Errorf("对象路径无效：%s", key)
		default:
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("对象路径为空")
	}
	return strings.Join(parts, "/"), nil
}
