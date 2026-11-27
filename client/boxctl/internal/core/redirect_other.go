//go:build !linux

package core

import "os"

// redirectOutput 在其他平台上把 os.Stdout、os.Stderr 换成日志文件（开发用）。
func redirectOutput(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	os.Stdout, os.Stderr = f, f
	return nil
}
