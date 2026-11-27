//go:build !darwin && !windows

package trust

// Watcher 在其他平台上不可用：只靠定时检查。
type Watcher struct{}

func NewWatcher(func()) (*Watcher, error) { return nil, nil }
func (w *Watcher) Start() error           { return nil }
func (w *Watcher) Physical() string       { return "" }
func (w *Watcher) Close()                 {}

func quickFingerprint(physical string) string {
	if physical != "" {
		return physical
	}
	return defaultInterface()
}
