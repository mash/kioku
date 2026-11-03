package buildinfo

import "runtime"

var Version = "dev"
var Commit = "unknown"
var BuiltAt = "unknown"

func Info() map[string]string {
	return map[string]string{"version": Version, "commit": Commit, "built_at": BuiltAt, "go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH}
}
