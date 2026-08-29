package profile

import "testing"

func TestHasOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/bb", false},
		{"/a/b", "/x/y", false},
		{"/a", "/a/b/c", true},
	}
	for _, c := range cases {
		if got := hasOverlap(c.a, c.b); got != c.want {
			t.Errorf("hasOverlap(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}
