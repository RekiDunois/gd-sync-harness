package filter

import "testing"

func TestExcludedPathPrefix(t *testing.T) {
	e := New("p", 0, []string{"exclude_path_prefix:Private"})
	cases := map[string]bool{
		"Private/secret.md": true,
		"Private":           true,
		"docs/Private/a.md": false,
		"Private2/a.md":     false,
		"notes/a.md":        false,
		"Docs/Private/file": false,
	}
	for in, want := range cases {
		got, _ := e.Excluded(in)
		if got != want {
			t.Errorf("Excluded(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestExcludedDirName(t *testing.T) {
	e := New("p", 0, []string{"exclude_dir_name:.git"})
	cases := map[string]bool{
		".git/config":      true,
		"a/.git/head":      true,
		"a/b/.git/x":       true,
		"gitfile":          false,
		"notes/.gitignore": false,
		"a/git/x.md":       false,
	}
	for in, want := range cases {
		got, _ := e.Excluded(in)
		if got != want {
			t.Errorf("Excluded(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestExcludedDirNameNested(t *testing.T) {
	e := New("p", 0, []string{"exclude_dir_name:Private"})
	if got, _ := e.Excluded("docs/Private/a.md"); !got {
		t.Error("dir_name should catch nested Private")
	}
}

func TestExcludedFileNameAndExt(t *testing.T) {
	e := New("p", 0, []string{
		"exclude_filename:.DS_Store",
		"exclude_extension:tmp",
		"exclude_extension:mp4",
	})
	cases := map[string]bool{
		".DS_Store":   true,
		"a/.DS_Store": true,
		"b.txt":       false,
		"c.tmp":       true,
		"d.TMP":       true,
		"e.mp4":       true,
		"f.MOV":       false,
		"x.c.tmp.bak": false,
	}
	for in, want := range cases {
		got, _ := e.Excluded(in)
		if got != want {
			t.Errorf("Excluded(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMaxFileSize(t *testing.T) {
	e := New("p", 1024, nil)
	if e.OverSize(1023) {
		t.Error("1023 should be under limit")
	}
	if !e.OverSize(1025) {
		t.Error("1025 should be over limit")
	}
	if e.OverSize(1024) {
		t.Error("1024 (== limit) should be under limit; limit is strict >")
	}
	e2 := New("p", 0, nil)
	if e2.OverSize(1 << 40) {
		t.Error("0 limit should mean unlimited")
	}
}

func TestNormalizeRelPath(t *testing.T) {
	got := NormalizeRelPath("/a/b", "/a/b/c/d.md")
	if got != "c/d.md" {
		t.Errorf("got %q", got)
	}
	if NormalizeRelPath("/a/b", "/a/b") != "" {
		t.Error("root should map to empty")
	}
	if NormalizeRelPath("/a/b", "/x/y") != "" {
		t.Error("outside source should be empty")
	}
}
