package launchd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
)

// JobKind distinguishes the per-profile jobs.
type JobKind string

const (
	JobWatch     JobKind = "watch"
	JobReconcile JobKind = "reconcile"
	JobWorker    JobKind = "worker"
)

// Config holds everything needed to render a plist.
type Config struct {
	LabelPrefix string
	ProfileID   string
	Kind        JobKind
	Binary      string
	LogDir      string
	ReconcileHour int
	ReconcileMin  int
}

// Label returns the launchd job label. For global jobs (worker, no profile)
// the label omits the profile segment.
func (c Config) Label() string {
	if c.ProfileID == "" {
		return fmt.Sprintf("%s.%s", c.LabelPrefix, c.Kind)
	}
	return fmt.Sprintf("%s.%s.%s", c.LabelPrefix, c.ProfileID, c.Kind)
}

// PlistFilename returns the plist filename for the job.
func (c Config) PlistFilename() string {
	return c.Label() + ".plist"
}

// PlistPath returns the absolute LaunchAgents path.
func (c Config) PlistPath(launchAgentsDir string) string {
	return filepath.Join(launchAgentsDir, c.PlistFilename())
}

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Binary}}</string>
		<string>{{.Kind}}</string>
		<string>{{.ProfileID}}</string>
	</array>
	<key>WorkingDirectory</key>
	<string>/</string>
	<key>RunAtLoad</key>
	{{if eq .Kind "watch"}}<true/>{{else if eq .Kind "worker"}}<true/>{{else}}<false/>{{end}}
	<key>KeepAlive</key>
	{{if eq .Kind "watch"}}<true/>{{else if eq .Kind "worker"}}<true/>{{else}}<false/>{{end}}
	<key>StandardOutPath</key>
	<string>{{.LogDir}}/{{.ProfileID}}.{{.Kind}}.log</string>
	<key>StandardErrorPath</key>
	<string>{{.LogDir}}/{{.ProfileID}}.{{.Kind}}.log</string>
	{{if eq .Kind "reconcile"}}
	<key>StartCalendarInterval</key>
	<dict>
		<key>Minute</key>
		<integer>{{.ReconcileMin}}</integer>
		<key>Hour</key>
		<integer>{{.ReconcileHour}}</integer>
	</dict>
	{{end}}
</dict>
</plist>
`

// Render produces the plist contents.
func (c Config) Render() ([]byte, error) {
	t := template.Must(template.New("plist").Parse(plistTemplate))
	var buf []byte
	b := newBuffer()
	if err := t.Execute(b, c); err != nil {
		return nil, err
	}
	buf = b.Bytes()
	return buf, nil
}

// Install writes the plist file.
func (c Config) Install(launchAgentsDir string) (string, error) {
	content, err := c.Render()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(launchAgentsDir, 0o755); err != nil {
		return "", err
	}
	path := c.PlistPath(launchAgentsDir)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Uninstall removes the plist and unloads via launchctl bootout.
func (c Config) Uninstall(launchAgentsDir string) error {
	path := c.PlistPath(launchAgentsDir)
	_ = launchctlBootout(c.Label())
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Load tells launchctl to load (bootstrap) the job.
func (c Config) Load(launchAgentsDir string) error {
	path := c.PlistPath(launchAgentsDir)
	if _, err := os.Stat(path); err != nil {
		return err
	}
	return launchctlBootstrap(path)
}

// Unload tells launchctl to unload (bootout) the job.
func (c Config) Unload() error {
	return launchctlBootout(c.Label())
}

func launchctlBootstrap(path string) error {
	out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid(), path).CombinedOutput()
	if err != nil {
		if isAlreadyLoaded(out) {
			return nil
		}
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
	}
	return nil
}

func launchctlBootout(label string) error {
	out, err := exec.Command("launchctl", "bootout", "gui/"+uid()+"/"+label).CombinedOutput()
	if err != nil {
		if isNotLoaded(out) {
			return nil
		}
		return fmt.Errorf("launchctl bootout: %w: %s", err, out)
	}
	return nil
}

func uid() string { return fmt.Sprintf("%d", os.Getuid()) }

func isAlreadyLoaded(out []byte) bool {
	s := lower(string(out))
	return contains(s, "already loaded") || contains(s, "service already loaded")
}

func isNotLoaded(out []byte) bool {
	s := lower(string(out))
	return contains(s, "could not find") || contains(s, "no such process") || contains(s, "bootout")
}

func lower(s string) string          { return stringsToLower(s) }
func contains(s, sub string) bool    { return stringsContains(s, sub) }
