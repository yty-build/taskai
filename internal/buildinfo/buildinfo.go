package buildinfo

import "fmt"

// These values are replaced with -ldflags in release builds.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Info describes the running binary.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Current returns the build information compiled into the binary.
func Current() Info {
	return Info{Version: Version, Commit: Commit, Date: Date}
}

func (i Info) String() string {
	return fmt.Sprintf("acpctl %s (commit %s, built %s)", i.Version, i.Commit, i.Date)
}
