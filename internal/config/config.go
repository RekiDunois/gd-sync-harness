package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type Operation int

const (
	FullSync Operation = iota
	FastUpsert
	DryRun
	Verify
)

type Config struct {
	Rclone RcloneConfig `json:"rclone"`
}
type RcloneConfig struct {
	GlobalArgs     []string `json:"global_args"`
	FullSyncArgs   []string `json:"full_sync_args"`
	FastUpsertArgs []string `json:"fast_upsert_args"`
	DryRunArgs     []string `json:"dry_run_args"`
	VerifyArgs     []string `json:"verify_args"`
}

var reserved = map[string]bool{
	"--config": true, "--files-from": true, "--files-from-raw": true,
	"--filter": true, "--include": true, "--exclude": true,
	"--delete-excluded": true, "--max-delete": true, "--track-renames": true,
	"--dry-run": true, "--use-json-log": true, "--stats": true,
	"--stats-log-level": true,
}

func Default() Config {
	return Config{Rclone: RcloneConfig{FullSyncArgs: []string{"--transfers=12"}, FastUpsertArgs: []string{"--transfers=4"}}}
}

func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Validate(cfg Config) error {
	sets := []struct {
		name string
		args []string
	}{
		{"rclone.global_args", cfg.Rclone.GlobalArgs}, {"rclone.full_sync_args", cfg.Rclone.FullSyncArgs},
		{"rclone.fast_upsert_args", cfg.Rclone.FastUpsertArgs}, {"rclone.dry_run_args", cfg.Rclone.DryRunArgs},
		{"rclone.verify_args", cfg.Rclone.VerifyArgs},
	}
	for _, set := range sets {
		for _, arg := range set.args {
			if !strings.HasPrefix(arg, "--") || arg == "--" {
				return fmt.Errorf("%s contains invalid argument %q: arguments must start with --", set.name, arg)
			}
			name := strings.SplitN(arg, "=", 2)[0]
			if reserved[name] {
				return fmt.Errorf("%s contains reserved flag %q; this flag is owned by knowledge-sync", set.name, name)
			}
		}
	}
	return nil
}

func ArgsFor(cfg Config, op Operation) []string {
	var specific []string
	switch op {
	case FullSync:
		specific = cfg.Rclone.FullSyncArgs
	case FastUpsert:
		specific = cfg.Rclone.FastUpsertArgs
	case DryRun:
		specific = cfg.Rclone.DryRunArgs
	case Verify:
		specific = cfg.Rclone.VerifyArgs
	}
	if specific == nil {
		switch op {
		case FullSync:
			specific = Default().Rclone.FullSyncArgs
		case FastUpsert:
			specific = Default().Rclone.FastUpsertArgs
		}
	}
	return merge(cfg.Rclone.GlobalArgs, specific)
}

func merge(global, specific []string) []string {
	out := append([]string{}, global...)
	positions := map[string]int{}
	for i, arg := range out {
		positions[strings.SplitN(arg, "=", 2)[0]] = i
	}
	for _, arg := range specific {
		name := strings.SplitN(arg, "=", 2)[0]
		if i, ok := positions[name]; ok {
			out[i] = arg
		} else {
			positions[name] = len(out)
			out = append(out, arg)
		}
	}
	return out
}
