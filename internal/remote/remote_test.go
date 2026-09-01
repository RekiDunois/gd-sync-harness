package remote

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/state"
)

func remoteRclone(t *testing.T) (*rcexec.Rclone, string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-rclone.sh")
	logDir := t.TempDir()
	log := filepath.Join(logDir, "rclone.log")
	mkdirLog := filepath.Join(logDir, "mkdir.log")
	count := filepath.Join(t.TempDir(), "lsjson-count")
	script := `#!/bin/sh
set -eu
command=""
first=""
second=""
capture=0
for arg in "$@"; do
  if [ "$capture" -eq 1 ]; then
    if [ -z "$first" ]; then first="$arg"; else second="$arg"; fi
  fi
  case "$arg" in
    listremotes|mkdir|lsjson|about) command="$arg"; capture=1 ;;
    config) command="config"; capture=0 ;;
  esac
done
printf '%s\n' "$*" >> "${FAKE_REMOTE_LOG:-/dev/null}"
case "$command" in
  listremotes)
    [ "${FAKE_REMOTE_MODE:-}" = "list_error" ] && exit 7
    printf '%b' "${FAKE_REMOTES:-example-drive:\n}" ;;
  config)
    [ "${FAKE_REMOTE_MODE:-}" = "backend_error" ] && exit 7
    printf 'type = %s\n' "${FAKE_BACKEND:-drive}" ;;
  mkdir)
    [ "${FAKE_REMOTE_MODE:-}" = "mkdir_error" ] && exit 9
    printf '%s\n' "$first" >> "${FAKE_MKDIR_LOG:-/dev/null}" ;;
  lsjson)
    case "${FAKE_REMOTE_MODE:-}" in
      inspect_missing) exit 3 ;;
      inspect_transport) exit 7 ;;
      inspect_malformed) printf 'not-json\n'; exit 0 ;;
    esac
    count=0
    if [ -f "${FAKE_LSJSON_COUNT:-}" ]; then count=$(awk 'NR == 1 {print $1}' "$FAKE_LSJSON_COUNT"); fi
    count=$((count + 1))
    printf '%s\n' "$count" > "${FAKE_LSJSON_COUNT:-/dev/null}"
    if [ "$count" -le "${FAKE_EMPTY_CALLS:-0}" ]; then printf '[]\n'; exit 0; fi
    case "${FAKE_REMOTE_MODE:-}" in
      tree)
        case "$first" in
          *:) printf '[{"Name":"root","ID":"root-id","IsDir":true},{"Name":"file","ID":"file-id","IsDir":false}]\n' ;;
          */root|*:root)
            if [ "${FAKE_DUPLICATE:-0}" = "1" ]; then
              printf '[{"Name":"child","ID":"child-1","IsDir":true},{"Name":"child","ID":"child-2","IsDir":true}]\n'
            else
              printf '[{"Name":"child","ID":"child-id","IsDir":true}]\n'
            fi ;;
          *) printf '[]\n' ;;
        esac ;;
      *) printf '%s\n' "${FAKE_LSJSON_OUTPUT:-[]}" ;;
    esac ;;
  about)
    [ "${FAKE_REMOTE_MODE:-}" = "about_error" ] && exit 7
    printf '%s\n' "$FAKE_ABOUT" ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_REMOTE_LOG", log)
	t.Setenv("FAKE_MKDIR_LOG", mkdirLog)
	t.Setenv("FAKE_ABOUT", `{"Total":100,"Used":20,"Free":80}`)
	t.Setenv("FAKE_LSJSON_COUNT", count)
	return &rcexec.Rclone{Binary: bin}, log
}

func remoteManager(t *testing.T) (*Manager, *rcexec.Rclone, string) {
	t.Helper()
	r, log := remoteRclone(t)
	m := New(r, nil)
	m.sleep = func(time.Duration) {}
	return m, r, log
}

func TestValidateRemoteNormalizesNamesAndChecksBackend(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		remotes string
		backend string
		mode    string
		want    string
		wantErr string
	}{
		{name: "drive with colon", remote: "example-drive:", remotes: "example-drive:\n", backend: "DRIVE", want: "drive"},
		{name: "missing", remote: "missing", remotes: "example-drive:\n", backend: "drive", wantErr: "not found"},
		{name: "wrong backend", remote: "example-drive", remotes: "example-drive:\n", backend: "s3", wantErr: "only google drive"},
		{name: "list error", remote: "example-drive", mode: "list_error", wantErr: "listremotes"},
		{name: "backend error", remote: "example-drive", remotes: "example-drive:\n", mode: "backend_error", wantErr: "config show"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := remoteManager(t)
			if tt.remotes != "" {
				t.Setenv("FAKE_REMOTES", tt.remotes)
			}
			if tt.backend != "" {
				t.Setenv("FAKE_BACKEND", tt.backend)
			}
			if tt.mode != "" {
				t.Setenv("FAKE_REMOTE_MODE", tt.mode)
			}
			got, err := m.ValidateRemote(context.Background(), tt.remote)
			if tt.wantErr == "" {
				if err != nil || got != tt.want {
					t.Fatalf("ValidateRemote = %q, %v; want %q", got, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateRemote error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveFolderIDWalksNestedFoldersAndRejectsRoot(t *testing.T) {
	m, _, _ := remoteManager(t)
	t.Setenv("FAKE_REMOTE_MODE", "tree")
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "single", path: "root", want: "root-id"},
		{name: "nested", path: "root/child", want: "child-id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.ResolveFolderID(context.Background(), "example-drive:", tt.path)
			if err != nil || got != tt.want {
				t.Fatalf("ResolveFolderID = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, err := m.ResolveFolderID(context.Background(), "example-drive", "/"); err == nil || !strings.Contains(err.Error(), "remote root") {
		t.Fatalf("root resolution error = %v", err)
	}

	t.Setenv("FAKE_LSJSON_OUTPUT", `[{"Name":"file","ID":"file-id","IsDir":false}]`)
	if _, err := m.ResolveFolderID(context.Background(), "example-drive", "missing"); err == nil || !strings.Contains(err.Error(), "after retries") {
		t.Fatalf("missing folder error = %v", err)
	}
}

func TestResolveFolderIDUsesExpectedRetryBackoff(t *testing.T) {
	m, _, _ := remoteManager(t)
	t.Setenv("FAKE_REMOTE_MODE", "tree")
	t.Setenv("FAKE_EMPTY_CALLS", "4")
	var waits []time.Duration
	m.sleep = func(d time.Duration) { waits = append(waits, d) }

	id, err := m.ResolveFolderID(context.Background(), "example-drive", "root/child")
	if err != nil || id != "child-id" {
		t.Fatalf("ResolveFolderID = %q, %v", id, err)
	}
	want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}
	if !reflect.DeepEqual(waits, want) {
		t.Fatalf("retry waits = %v, want %v", waits, want)
	}
}

func TestResolveFolderIDStrictAndBinding(t *testing.T) {
	tests := []struct {
		name      string
		duplicate bool
		path      string
		want      string
		wantErr   string
	}{
		{name: "unique nested", path: "root/child", want: "child-id"},
		{name: "duplicate", duplicate: true, path: "root/child", wantErr: "exactly one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := remoteManager(t)
			t.Setenv("FAKE_REMOTE_MODE", "tree")
			if tt.duplicate {
				t.Setenv("FAKE_DUPLICATE", "1")
			}
			got, err := m.ResolveFolderIDStrict(context.Background(), "example-drive:", tt.path)
			if tt.wantErr == "" {
				if err != nil || got != tt.want {
					t.Fatalf("strict resolution = %q, %v; want %q", got, err, tt.want)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("strict resolution error = %v, want %q", err, tt.wantErr)
			}
		})
	}

	m, _, _ := remoteManager(t)
	t.Setenv("FAKE_REMOTE_MODE", "tree")
	p := &state.Profile{RemoteName: "example-drive", RemoteDisplayPath: "root", RemoteFolderID: "root-id"}
	if err := m.ValidateFolderBinding(context.Background(), p); err != nil {
		t.Fatalf("valid binding: %v", err)
	}
	p.RemoteFolderID = "other-id"
	if err := m.ValidateFolderBinding(context.Background(), p); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed binding error = %v", err)
	}
}

func TestResolveFolderIDAndInspectReportFailures(t *testing.T) {
	m, _, _ := remoteManager(t)
	t.Setenv("FAKE_REMOTE_MODE", "inspect_malformed")
	if _, err := m.ResolveFolderIDStrict(context.Background(), "example-drive", "root"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("malformed strict output error = %v", err)
	}

	tests := []struct {
		name      string
		mode      string
		output    string
		exists    bool
		nonEmpty  bool
		wantError bool
	}{
		{name: "missing", mode: "inspect_missing"},
		{name: "empty", mode: "inspect_empty", output: "[]", exists: true},
		{name: "non-empty", mode: "inspect_nonempty", output: `[{"Name":"child"}]`, exists: true, nonEmpty: true},
		{name: "malformed", mode: "inspect_malformed", wantError: true},
		{name: "transport", mode: "inspect_transport", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := remoteManager(t)
			t.Setenv("FAKE_REMOTE_MODE", tt.mode)
			if tt.output != "" {
				t.Setenv("FAKE_LSJSON_OUTPUT", tt.output)
			}
			exists, nonEmpty, err := m.InspectPath(context.Background(), "example-drive:", "root")
			if tt.wantError {
				if err == nil {
					t.Fatal("InspectPath succeeded, want error")
				}
				return
			}
			if err != nil || exists != tt.exists || nonEmpty != tt.nonEmpty {
				t.Fatalf("InspectPath = %v, %v, %v", exists, nonEmpty, err)
			}
		})
	}
}

func TestCreateManagedRootRunsMkdirThenResolvesID(t *testing.T) {
	m, _, log := remoteManager(t)
	t.Setenv("FAKE_REMOTE_MODE", "tree")
	id, err := m.CreateManagedRoot(context.Background(), "example-drive", "root/child")
	if err != nil || id != "child-id" {
		t.Fatalf("CreateManagedRoot = %q, %v", id, err)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(log), "mkdir.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "example-drive:root/child\n" {
		t.Fatalf("mkdir args = %q", b)
	}

	t.Setenv("FAKE_REMOTE_MODE", "mkdir_error")
	if _, err := m.CreateManagedRoot(context.Background(), "example-drive", "root"); err == nil || !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("mkdir failure = %v", err)
	}
}

func TestCheckQuotaStoresAllOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		about  string
		mode   string
		warn   int64
		status string
		err    bool
	}{
		{name: "ok", about: `{"Total":100,"Used":20,"Free":80}`, warn: 10, status: state.QuotaOK},
		{name: "low", about: `{"Total":100,"Used":95,"Free":5}`, warn: 10, status: state.QuotaLow},
		{name: "full", about: `{"Total":100,"Used":100,"Free":0}`, warn: 10, status: state.QuotaFull},
		{name: "unknown", mode: "about_error", status: state.QuotaUnknown, err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := remoteRclone(t)
			t.Setenv("FAKE_ABOUT", tt.about)
			if tt.mode != "" {
				t.Setenv("FAKE_REMOTE_MODE", tt.mode)
			}
			db, err := state.Open(filepath.Join(t.TempDir(), "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			m := New(r, db)
			got, err := m.CheckQuota(context.Background(), "example-drive:", tt.warn)
			if (err != nil) != tt.err || got.QuotaStatus != tt.status || got.RemoteName != "example-drive" {
				t.Fatalf("CheckQuota = %+v, %v", got, err)
			}
			stored, err := db.GetRemote("example-drive")
			if err != nil {
				t.Fatal(err)
			}
			if stored.QuotaStatus != tt.status || stored.RemoteName != got.RemoteName {
				t.Fatalf("stored remote = %+v, want %+v", stored, got)
			}
			if !tt.err && (stored.TotalBytes != 100 || stored.UsedBytes < 0 || stored.FreeBytes < 0) {
				t.Fatalf("stored quota values = %+v", stored)
			}
		})
	}
}

func TestConsistencyRetryDelay(t *testing.T) {
	for attempt, want := range []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond} {
		if got := consistencyRetryDelay(attempt); got != want {
			t.Fatalf("attempt %d delay = %s, want %s", attempt, got, want)
		}
	}
}
