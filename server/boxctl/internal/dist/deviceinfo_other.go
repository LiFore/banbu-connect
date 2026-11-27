//go:build !darwin && !windows

package dist

import "runtime"

func computerName() string { return "" }

func osVersion() string { return runtime.GOOS }
