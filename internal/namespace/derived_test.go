package namespace

import "testing"

func TestIsDerivedPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{".knowledge-derived", true},
		{".knowledge-derived/README.md", true},
		{"notes/.knowledge-derived", false},
		{".knowledge-derived-other/file", false},
	} {
		if got := IsDerivedPath(tc.path); got != tc.want {
			t.Fatalf("IsDerivedPath(%q) = %t, want %t", tc.path, got, tc.want)
		}
	}
}
