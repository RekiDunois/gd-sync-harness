// Package namespace defines system-owned path reservations shared by all data
// lanes. These reservations are not user-configurable ignore rules.
package namespace

import "strings"

const DerivedRoot = ".knowledge-derived"

// IsDerivedPath reports whether rel is the reserved derived namespace or one of
// its descendants. It intentionally does not clean or normalize rel.
func IsDerivedPath(rel string) bool {
	return rel == DerivedRoot || strings.HasPrefix(rel, DerivedRoot+"/")
}
