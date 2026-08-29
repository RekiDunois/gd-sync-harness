package launchd

import (
	"bytes"
	"strings"
)

func newBuffer() *bytes.Buffer       { return &bytes.Buffer{} }
func stringsToLower(s string) string { return strings.ToLower(s) }
func stringsContains(s, sub string) bool {
	return strings.Contains(s, sub)
}
