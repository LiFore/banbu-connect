//go:build !darwin && !windows

package killswitch

func NewSystemGuard() DNSGuard { return &FakeGuard{} }
