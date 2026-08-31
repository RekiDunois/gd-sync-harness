package cli

import (
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	knowledgecompiler "knowledge-sync/internal/compiler"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

func TestCompilerCLIWithMathProtection(t *testing.T) {
	home := t.TempDir()
	sourceRoot := t.TempDir()
	bin := filepath.Join(t.TempDir(), "knowledge-sync")

	writeCLICompilerFile(t, sourceRoot, "target.md", "target")
	writeCLICompilerFile(t, sourceRoot, "note.md", "[[target]] #visible\n$x #fake [[fake]]$\n$$\n#display-fake [[display-fake]]\n$$\n")
	original := map[string][]byte{}
	for _, rel := range []string{"target.md", "note.md"} {
		content, err := os.ReadFile(filepath.Join(sourceRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		original[rel] = content
	}

	dbPath := filepath.Join(home, ".local", "share", "knowledge-sync", "knowledge-sync.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	p := &state.Profile{
		ID: "smoke", ProfileUUID: "smoke-uuid", Type: "generic", SourcePath: sourceRoot,
		RemoteName: "example-drive", RemoteFolderID: "example-folder", RemoteDisplayPath: "example-path",
		MaxDelete: 100,
	}
	if err := db.CreateProfileWithPolicy(p, &policy.Snapshot{}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	build := osexec.Command("go", "build", "-o", bin, "knowledge-sync/cmd/knowledge-sync")
	build.Dir = moduleRoot(t)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	env := append(os.Environ(), "HOME="+home)
	compile := osexec.Command(bin, "compile", "smoke")
	compile.Env = env
	compileOutput, err := compile.CombinedOutput()
	if err != nil {
		t.Fatalf("compiler CLI: %v\n%s", err, compileOutput)
	}
	t.Logf("compile output:\n%s", compileOutput)

	status := osexec.Command(bin, "compiler", "status", "smoke", "--verify")
	status.Env = env
	statusOutput, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("compiler status CLI: %v\n%s", err, statusOutput)
	}
	t.Logf("status output:\n%s", statusOutput)
	if !strings.Contains(string(statusOutput), "local integrity: verified") {
		t.Fatalf("status output = %s", statusOutput)
	}

	compilerRoot := filepath.Join(home, ".local", "share", "knowledge-sync", "compiler", "smoke-uuid")
	store := knowledgecompiler.NewStore(compilerRoot)
	if _, err := store.VerifyCurrent(); err != nil {
		t.Fatalf("verify current generation: %v", err)
	}
	pointer, err := store.Current()
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(compilerRoot, "generations", pointer.CurrentGenerationID, "MANIFEST.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest knowledgecompiler.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Counts.Links != 1 || manifest.Counts.Tags != 1 || manifest.Counts.BrokenLinks != 0 {
		t.Fatalf("manifest counts = %+v", manifest.Counts)
	}
	for rel, want := range original {
		got, err := os.ReadFile(filepath.Join(sourceRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("source %s changed during CLI compile", rel)
		}
	}
}

func writeCLICompilerFile(t *testing.T, root, rel, content string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(workingDir, "..", "..")
}
