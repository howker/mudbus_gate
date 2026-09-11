// Package buildinfo exposes a small runtime passport of the running binary.
// It intentionally relies on Go's embedded build information for revision,
// dirty state and toolchain, so the values describe the actual executable,
// not a separate version file that can drift out of sync.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// BuildTime is injected by the production build command via -ldflags -X.
// An empty value means the binary was built without that release stamp.
var BuildTime string

// Info is the operator-visible passport of the running executable.
type Info struct {
	Revision  string
	Modified  bool
	BuildTime string
	GoVersion string
}

// Current returns the passport embedded in the currently running binary.
func Current() Info {
	out := Info{
		BuildTime: strings.TrimSpace(BuildTime),
		GoVersion: runtime.Version(),
	}

	if bi, ok := debug.ReadBuildInfo(); ok {
		if strings.TrimSpace(bi.GoVersion) != "" {
			out.GoVersion = bi.GoVersion
		}
		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs.revision":
				out.Revision = strings.TrimSpace(setting.Value)
			case "vcs.modified":
				out.Modified = strings.EqualFold(strings.TrimSpace(setting.Value), "true")
			}
		}
	}
	return out
}

// ShortRevision is compact enough for the UI while the API still returns the
// full revision for diagnostics and exact comparison.
func (i Info) ShortRevision() string {
	if len(i.Revision) <= 12 {
		return i.Revision
	}
	return i.Revision[:12]
}
