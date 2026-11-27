package desktop

import "context"

// Windows 的系统代理在 HKCU 下，提权后的进程可能是另一个用户，由外壳在提权前清除（client/app/src-tauri/src/main.rs）。
func clearLoopbackProxy(context.Context) error { return nil }
