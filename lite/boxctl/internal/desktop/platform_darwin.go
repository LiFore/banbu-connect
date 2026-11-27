package desktop

import (
	"os"
	"path/filepath"
)

// dataDir 是当前用户的数据目录：后台进程以用户身份运行，由外壳拉起，不需要管理员权限。
func dataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, "Library", "Application Support", dirName)
}
