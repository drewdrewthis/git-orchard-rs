package release

import "runtime/debug"

// Revision returns this binary's VCS revision from the embedded build info,
// with a "+dirty" suffix when the working tree was modified at build time, or
// "" when the build carried no VCS stamp (e.g. -buildvcs=false).
//
// orchard-shell's doctor compares it across suite binaries: two binaries built
// from the same commit report the same revision, while a stale/prototype build
// reports a different one — the exact skew that silently broke sidebar clicks
// (orchardist#787) yet can hide behind an identical --version "dev".
func Revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" && dirty {
		rev += "+dirty"
	}
	return rev
}
