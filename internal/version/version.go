// Package version exposes build-time version information.
package version

import (
	"runtime/debug"
)

var UserAgent = func() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return "apoci/" + info.Main.Version
	}
	return "apoci/dev"
}()
