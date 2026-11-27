//go:build !windows && !darwin

package killswitch

import (
	"context"
	"errors"
)

// 路由盒用 internal/guard；这个包只支持桌面平台。
var errUnsupported = errors.New("当前平台不支持")

type unsupportedBackend struct{}

func NewSystemBackend() Backend { return unsupportedBackend{} }

func (unsupportedBackend) Interfaces(context.Context) ([]string, error) { return nil, errUnsupported }
func (unsupportedBackend) Inspect(context.Context, Desired) (Actual, error) {
	return Actual{}, errUnsupported
}
func (unsupportedBackend) Apply(context.Context, Desired) error { return errUnsupported }
func (unsupportedBackend) Remove(context.Context) error         { return errUnsupported }
func (unsupportedBackend) FlushDNS(context.Context) error       { return errUnsupported }
func (unsupportedBackend) ValidateListen(string, int) error     { return nil }
