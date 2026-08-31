package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// Store manages one profile's app-owned immutable compiler generations.
type Store struct{ Root string }

func NewStore(root string) *Store { return &Store{Root: root} }

// Publish commits a generation directory and then atomically replaces the
// local current pointer. MANIFEST.json is written last inside the generation.
func (s *Store) Publish(result *BuildResult) (*RootPointer, error) {
	return s.PublishWithProtected(result, "")
}

// PublishWithProtected commits a generation while retaining an in-flight
// generation from compiler GC.
func (s *Store) PublishWithProtected(result *BuildResult, protectedGeneration string) (*RootPointer, error) {
	if s == nil || s.Root == "" {
		return nil, fmt.Errorf("compiler store root is required")
	}
	if result == nil || result.Manifest.CompileGenerationID == "" {
		return nil, fmt.Errorf("generation result is required")
	}
	if !safeComponent(result.Manifest.CompileGenerationID) {
		return nil, fmt.Errorf("invalid generation id")
	}
	if err := os.MkdirAll(filepath.Join(s.Root, "generations"), 0o755); err != nil {
		return nil, err
	}
	stagingRoot := filepath.Join(s.Root, "staging")
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return nil, err
	}
	stage := filepath.Join(stagingRoot, result.Manifest.CompileGenerationID)
	if err := os.RemoveAll(stage); err != nil {
		return nil, fmt.Errorf("clear compiler staging: %w", err)
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(result.Artifacts))
	for name := range result.Artifacts {
		if name == "" || name == "MANIFEST.json" || filepath.IsAbs(name) || filepath.Clean(name) != name || filepath.Dir(name) != "." {
			return nil, fmt.Errorf("invalid generation artifact name %q", name)
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		if err := writeSynced(filepath.Join(stage, name), result.Artifacts[name]); err != nil {
			return nil, fmt.Errorf("write artifact %s: %w", name, err)
		}
	}
	result.Manifest.Artifacts = artifactInventory(result.Artifacts)
	manifest, err := json.MarshalIndent(result.Manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifest = append(manifest, '\n')
	if err := writeSynced(filepath.Join(stage, "MANIFEST.json"), manifest); err != nil {
		return nil, fmt.Errorf("write generation manifest: %w", err)
	}
	if err := syncDir(stage); err != nil {
		return nil, err
	}
	generationDir := filepath.Join(s.Root, "generations", result.Manifest.CompileGenerationID)
	if err := os.Rename(stage, generationDir); err != nil {
		return nil, fmt.Errorf("publish generation directory: %w", err)
	}
	if err := syncDir(filepath.Join(s.Root, "generations")); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(manifest)
	pointer := &RootPointer{
		SchemaVersion: SchemaVersion, ProfileID: result.Manifest.ProfileID,
		ProfileUUID: result.Manifest.ProfileUUID, CurrentGenerationID: result.Manifest.CompileGenerationID,
		GenerationManifestPath:  filepath.ToSlash(filepath.Join("generations", result.Manifest.CompileGenerationID, "MANIFEST.json")),
		GenerationManifestBytes: len(manifest), GenerationManifestSHA256: hex.EncodeToString(sum[:]),
		PublishedAt: result.Manifest.CompiledAt,
	}
	pointerBytes, err := json.MarshalIndent(pointer, "", "  ")
	if err != nil {
		return nil, err
	}
	pointerBytes = append(pointerBytes, '\n')
	if err := atomicReplace(filepath.Join(s.Root, "MANIFEST.json"), pointerBytes); err != nil {
		return nil, fmt.Errorf("commit compiler current pointer: %w", err)
	}
	_ = s.bestEffortGC(pointer.CurrentGenerationID, protectedGeneration)
	return pointer, nil
}

// VerifyCurrent validates the pointer, generation manifest and every detail
// artifact without consulting a remote.
func (s *Store) VerifyCurrent() (*RootPointer, error) {
	pointer, err := s.Current()
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(s.Root, filepath.FromSlash(pointer.GenerationManifestPath))
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(manifestBytes)
	if hex.EncodeToString(sum[:]) != pointer.GenerationManifestSHA256 || len(manifestBytes) != pointer.GenerationManifestBytes {
		return nil, fmt.Errorf("current generation manifest does not match root pointer")
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, err
	}
	if manifest.CompileGenerationID != pointer.CurrentGenerationID {
		return nil, fmt.Errorf("current pointer generation does not match manifest")
	}
	generationDir := filepath.Dir(manifestPath)
	for name, info := range manifest.Artifacts {
		if filepath.IsAbs(name) || filepath.Clean(name) != name || filepath.Dir(name) != "." || name == "MANIFEST.json" {
			return nil, fmt.Errorf("invalid manifest artifact name %q", name)
		}
		data, err := os.ReadFile(filepath.Join(generationDir, name))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		if len(data) != info.Bytes || hex.EncodeToString(sum[:]) != info.SHA256 {
			return nil, fmt.Errorf("artifact %q failed manifest integrity check", name)
		}
	}
	return pointer, nil
}

// VerifyGeneration validates one immutable generation without requiring it to
// be the current root pointer. This is used for an in-flight DerivedSync pin.
func (s *Store) VerifyGeneration(generationID string) error {
	if !safeComponent(generationID) {
		return fmt.Errorf("invalid generation id")
	}
	manifestPath := filepath.Join(s.Root, "generations", generationID, "MANIFEST.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return err
	}
	if manifest.CompileGenerationID != generationID {
		return fmt.Errorf("generation manifest does not match target")
	}
	for name, info := range manifest.Artifacts {
		if filepath.IsAbs(name) || filepath.Clean(name) != name || filepath.Dir(name) != "." || name == "MANIFEST.json" {
			return fmt.Errorf("invalid manifest artifact name %q", name)
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), name))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if len(data) != info.Bytes || hex.EncodeToString(sum[:]) != info.SHA256 {
			return fmt.Errorf("artifact %q failed manifest integrity check", name)
		}
	}
	return nil
}

func (s *Store) Current() (*RootPointer, error) {
	data, err := os.ReadFile(filepath.Join(s.Root, "MANIFEST.json"))
	if err != nil {
		return nil, err
	}
	var pointer RootPointer
	if err := json.Unmarshal(data, &pointer); err != nil {
		return nil, err
	}
	if pointer.SchemaVersion != SchemaVersion || pointer.CurrentGenerationID == "" {
		return nil, fmt.Errorf("invalid compiler root pointer")
	}
	return &pointer, nil
}

func (s *Store) bestEffortGC(current, protected string) error {
	entries, err := os.ReadDir(filepath.Join(s.Root, "generations"))
	if err != nil {
		return err
	}
	type generation struct {
		name string
		mod  int64
	}
	var dirs []generation
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == current || entry.Name() == protected {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		dirs = append(dirs, generation{name: entry.Name(), mod: info.ModTime().UnixNano()})
	}
	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].mod != dirs[j].mod {
			return dirs[i].mod > dirs[j].mod
		}
		return dirs[i].name > dirs[j].name
	})
	if len(dirs) > 1 {
		for _, entry := range dirs[1:] {
			if err := os.RemoveAll(filepath.Join(s.Root, "generations", entry.name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeSynced(name string, data []byte) error {
	file, err := os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func atomicReplace(name string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, name); err != nil {
		return err
	}
	return syncDir(filepath.Dir(name))
}

func syncDir(name string) error {
	dir, err := os.Open(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

func safeComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !filepath.IsAbs(value)
}
