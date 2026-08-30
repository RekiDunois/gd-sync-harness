package version

import (
	"runtime/debug"
	"strings"
)

const (
	Name    = "knowledge-sync"
	Version = "0.2.0"
	Build   = "async-reconcile"
)

// String identifies the source revision and whether the binary was built from
// a dirty worktree. This is emitted by long-running launchd processes so an
// on-disk binary cannot be confused with an already-running old process.
func String() string {
	return Name + " " + Details()
}

// Details returns the version text without the executable name, for CLI
// frameworks that prepend their own name to --version output.
func Details() string {
	parts := []string{Version, Build}
	if info, ok := debug.ReadBuildInfo(); ok {
		var revision, modified string
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				modified = setting.Value
			}
		}
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if revision != "" {
			parts = append(parts, "revision="+revision)
		}
		if modified != "" {
			parts = append(parts, "modified="+modified)
		}
	}
	return strings.Join(parts, " ")
}
