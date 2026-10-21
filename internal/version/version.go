// Package version exposes build identification for the devatlas binary.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is the human-readable release version. It is "dev" for local builds
// and can be overridden at link time:
//
//	go build -ldflags "-X github.com/Ryttis/devatlas/internal/version.Version=0.1.0" ./cmd/devatlas
var Version = "dev"

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Modified  bool   `json:"modified"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Current returns the build information of the running binary. The commit is
// read from the Go build info embedded by the toolchain when the module is
// built from a Git checkout; it is "unknown" otherwise.
func Current() Info {
	info := Info{
		Version:   Version,
		Commit:    "unknown",
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 12 {
				info.Commit = s.Value[:12]
			} else if s.Value != "" {
				info.Commit = s.Value
			}
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	return info
}

// String renders the build information on one line, e.g.
// "devatlas dev (commit 9a66817abcde, dirty) go1.27.3 linux/amd64".
func (i Info) String() string {
	commit := i.Commit
	if i.Modified {
		commit += ", dirty"
	}
	return fmt.Sprintf("devatlas %s (commit %s) %s %s/%s", i.Version, commit, i.GoVersion, i.OS, i.Arch)
}
