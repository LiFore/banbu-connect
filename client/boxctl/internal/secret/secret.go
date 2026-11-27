// Package secret 用设备密钥加密存储凭据（M1-08）。
//
// 密文格式：version(1B)=0x01 | nonce(12B) | AES-256-GCM 密文+tag。
// 每条密文绑定一个 AAD（如 "proxy_nodes.secret:<id>"），防止密文在行之间被挪用。
//
// 局限：设备没有 TPM，密钥与数据库在同一块盘上；这里防的是数据库文件
// 单独外泄（备份、诊断包、误拷贝），不防拿到整盘的攻击者。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	keySize       = 32
	formatVersion = 0x01
)

var ErrDecrypt = errors.New("secret: decrypt failed")

type Box struct {
	aead cipher.AEAD
}

// LoadOrCreateKey 读取设备密钥；文件不存在时生成并以 0600 写入。
func LoadOrCreateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != keySize {
			return nil, fmt.Errorf("secret: key %s has %d bytes, want %d", path, len(key), keySize)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// O_EXCL：并发首启时不覆盖已生成的密钥
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}
	return key, f.Close()
}

func New(key []byte) (*Box, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("secret: key must be %d bytes", keySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plaintext []byte, aad string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, formatVersion)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, []byte(aad)), nil
}

func (b *Box) Open(ciphertext []byte, aad string) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(ciphertext) < 1+ns+b.aead.Overhead() || ciphertext[0] != formatVersion {
		return nil, ErrDecrypt
	}
	plain, err := b.aead.Open(nil, ciphertext[1:1+ns], ciphertext[1+ns:], []byte(aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}

// Mask 用于 API 默认返回值：只表达“有/无”，不暴露内容和长度。
func Mask(s string) string {
	if s == "" {
		return ""
	}
	return "******"
}
