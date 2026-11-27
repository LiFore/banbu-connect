//go:build !windows && !darwin

package trust

import "context"

type systemDetector struct{}

// NewSystemDetector 在不支持的平台上只报告出口网卡，永远不是 Wi-Fi，因此始终阻断。
func NewSystemDetector() Detector { return systemDetector{} }

func (systemDetector) Detect(context.Context) Network {
	return Network{Interface: defaultInterface(), Hint: "当前平台不支持读取 Wi-Fi"}
}
