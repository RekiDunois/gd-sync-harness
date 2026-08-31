package derived

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/paths"
	"knowledge-sync/internal/remote"
	"knowledge-sync/internal/sidecar"
	"knowledge-sync/internal/state"
)

const (
	derivedRoot = ".knowledge-derived"
	phaseSync   = "sync"
	phaseCheck  = "check"
	phaseCommit = "commit"
	phasePurge  = "purge"
)

// Publisher owns the correctness algorithm for the compiler-derived lane. It
// deliberately does not consume ordinary sync tuning flags.
type Publisher struct {
	Rclone *exec.Rclone
	Remote *remote.Manager
}

func NewPublisher(r *exec.Rclone, db *state.DB) *Publisher {
	return &Publisher{Rclone: r, Remote: remote.New(r, db)}
}

// Publish executes detail sync, complete content check, and MANIFEST commit.
func (p *Publisher) Publish(ctx context.Context, profile *state.Profile, generationID string, onStats func(exec.ProgressStats), setPhase func(string)) error {
	binding := state.DerivedBindingFingerprint(profile.RemoteName, profile.RemoteFolderID)
	if _, err := p.validateOwnership(ctx, profile, true, binding); err != nil {
		return err
	}
	root, err := paths.CompilerRoot(profile.ProfileUUID)
	if err != nil {
		return err
	}
	local := filepath.Join(root, "generations", generationID)
	if _, err := os.Stat(filepath.Join(local, "MANIFEST.json")); err != nil {
		return fmt.Errorf("derived generation %q unavailable: %w", generationID, err)
	}
	remoteRoot := p.remoteDerivedPath(profile)

	setPhase(phaseSync)
	if res := p.Rclone.RunProgress(ctx, onStats, "sync", "--exclude", "/MANIFEST.json", "--fast-list", "--track-renames", local, remoteRoot); res.Err != nil {
		return fmt.Errorf("derived detail sync: %w: %s", res.Err, res.StderrTrimmed())
	}
	setPhase(phaseCheck)
	if res := p.Rclone.RunProgress(ctx, onStats, "check", "--exclude", "/MANIFEST.json", local, remoteRoot); res.Err != nil {
		return fmt.Errorf("derived detail check: %w: %s", res.Err, res.StderrTrimmed())
	}
	setPhase(phaseCommit)
	manifest := filepath.Join(local, "MANIFEST.json")
	manifestRemote := remoteRoot + "/MANIFEST.json"
	if res := p.Rclone.Run(ctx, "copyto", manifest, manifestRemote); res.Err != nil {
		return fmt.Errorf("derived manifest commit: %w: %s", res.Err, res.StderrTrimmed())
	}
	return nil
}

// Purge removes only the compiler-derived namespace after ownership checks.
func (p *Publisher) Purge(ctx context.Context, profile *state.Profile, setPhase func(string)) error {
	binding := state.DerivedBindingFingerprint(profile.RemoteName, profile.RemoteFolderID)
	claimed, err := p.validateOwnership(ctx, profile, false, binding)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	exists, _, err := p.Remote.InspectPath(ctx, profile.RemoteName, p.remoteDerivedPath(profile))
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	setPhase(phasePurge)
	res := p.Rclone.Run(ctx, "purge", p.remoteDerivedPath(profile))
	if res.Err != nil {
		return fmt.Errorf("derived purge: %w: %s", res.Err, res.StderrTrimmed())
	}
	return nil
}

// validateOwnership returns whether a valid derived sidecar was present. A
// publish may claim an absent/empty namespace; purge may safely prove absence
// without claiming it.
func (p *Publisher) validateOwnership(ctx context.Context, profile *state.Profile, claim bool, binding string) (bool, error) {
	if err := sidecar.Validate(ctx, p.Rclone, profile, profile.RemoteDisplayPath); err != nil {
		return false, err
	}
	if err := p.Remote.ValidateFolderBinding(ctx, profile); err != nil {
		return false, fmt.Errorf("derived live binding: %w", err)
	}
	exists, nonEmpty, err := p.Remote.InspectPath(ctx, profile.RemoteName, p.remoteDerivedPath(profile))
	if err != nil {
		return false, err
	}
	derivedExists, err := p.derivedSidecarExists(ctx, profile)
	if err != nil {
		return false, err
	}
	if derivedExists {
		value, err := sidecar.ReadDerived(ctx, p.Rclone, profile.RemoteName, profile.ProfileUUID)
		if err != nil {
			return false, err
		}
		if err := sidecar.ValidateDerived(value, profile, binding); err != nil {
			return false, err
		}
		return true, nil
	}
	if !claim {
		if nonEmpty {
			return false, fmt.Errorf("derived namespace is non-empty but unclaimed")
		}
		return false, nil
	}
	if exists && nonEmpty {
		return false, fmt.Errorf("derived namespace is non-empty but unclaimed")
	}
	if res := p.Rclone.Run(ctx, "mkdir", strings.TrimSuffix(profile.RemoteName, ":")+":"+sidecar.RemoteMetadataRoot+"/derived"); res.Err != nil {
		return false, fmt.Errorf("create derived metadata directory: %w: %s", res.Err, res.StderrTrimmed())
	}
	if err := sidecar.WriteDerived(ctx, p.Rclone, profile.RemoteName, sidecar.CreateDerived(profile, binding)); err != nil {
		return false, err
	}
	value, err := sidecar.ReadDerived(ctx, p.Rclone, profile.RemoteName, profile.ProfileUUID)
	if err != nil {
		return false, fmt.Errorf("derived sidecar read-back: %w", err)
	}
	if err := sidecar.ValidateDerived(value, profile, binding); err != nil {
		return false, err
	}
	return true, nil
}

func (p *Publisher) derivedSidecarExists(ctx context.Context, profile *state.Profile) (bool, error) {
	res := p.Rclone.Run(ctx, "lsf", profile.RemoteName+":"+sidecar.RemoteMetadataRoot+"/derived", "--files-only")
	if res.Err != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(res.Err, &exitErr) && exitErr.ExitCode() == 3 {
			return false, nil
		}
		return false, fmt.Errorf("inspect derived sidecar: %w: %s", res.Err, res.StderrTrimmed())
	}
	for _, line := range strings.Split(res.StdoutTrimmed(), "\n") {
		if strings.TrimSpace(line) == profile.ProfileUUID+".json" {
			return true, nil
		}
	}
	return false, nil
}

func (p *Publisher) remoteDerivedPath(profile *state.Profile) string {
	return strings.TrimSuffix(profile.RemoteName, ":") + ":" + strings.TrimSuffix(profile.RemoteDisplayPath, "/") + "/" + derivedRoot
}

// ManifestGenerationID checks the local manifest without trusting a caller's
// generation path for the actual publication target.
func ManifestGenerationID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var value struct {
		CompileGenerationID string `json:"compile_generation_id"`
	}
	if err := json.Unmarshal(b, &value); err != nil {
		return "", err
	}
	if value.CompileGenerationID == "" {
		return "", fmt.Errorf("manifest has no compile generation id")
	}
	return value.CompileGenerationID, nil
}
