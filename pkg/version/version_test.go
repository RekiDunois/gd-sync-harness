package version

import (
	"strings"
	"testing"
)

func TestVersionStringsHaveStablePrefixes(t *testing.T) {
	if got, want := String(), Name+" "+Version+" "+Build; !strings.HasPrefix(got, want) {
		t.Fatalf("String = %q, want prefix %q", got, want)
	}
	if got := Details(); !strings.HasPrefix(got, Version+" "+Build) {
		t.Fatalf("Details = %q", got)
	}
}
