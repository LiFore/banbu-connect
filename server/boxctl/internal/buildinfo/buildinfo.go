// Package buildinfo 保存构建时通过 -ldflags 注入的版本信息。
package buildinfo

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
