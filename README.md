# knowledge-sync

[![CI](https://github.com/RekiDunois/gd-sync-harness/actions/workflows/ci.yml/badge.svg)](https://github.com/RekiDunois/gd-sync-harness/actions/workflows/ci.yml)

`knowledge-sync` mirrors local knowledge directories to Google Drive through `rclone`. It keeps durable per-profile state, runs synchronization through a background worker, supports `.gitignore`-style exclusion policy, provides explicit safety gates for remote deletion, and can compile an eligible source corpus into derived immutable generations.

The repository is currently macOS-oriented for day-to-day operation: managed background jobs are installed with `launchd`, logs live under `~/Library/Logs`, and file watching uses `fswatch`. The Go test suite also runs on Linux in CI.

## Features

- Multiple independent sync profiles backed by a local SQLite database.
- Google Drive transport through an existing `rclone` remote.
- Asynchronous initial sync and manual reconciliation owned by a single background worker.
- Live sync progress with `knowledge-sync profile status <id>`.
- `fswatch`-driven change detection plus scheduled reconciliation.
- Committed `.gitignore` policy snapshots instead of implicit live policy changes.
- Explicit prune preview/authorization flow for deleting newly suppressed remote objects.
- Per-profile deletion budgets and ownership validation before destructive operations.
- Local compiler generations and worker-published derived output.
- Verification, duplicate inspection, emergency stop, state backups, and diagnostics.

## Requirements

For normal macOS use:

- Go compatible with `go.mod` (currently Go 1.26.7).
- [`rclone`](https://rclone.org/) with a Google Drive remote configured.
- [`fswatch`](https://emcrisostomo.github.io/fswatch/) for managed filesystem watching.

With Homebrew:

```bash
brew install go rclone fswatch
rclone config
```

`knowledge-sync` discovers the active rclone config using `rclone config file` and persists resolved executable paths so `launchd` jobs do not depend on an interactive shell `PATH`.

## Build

```bash
git clone https://github.com/RekiDunois/gd-sync-harness.git
cd gd-sync-harness
go build -trimpath -o knowledge-sync ./cmd/knowledge-sync
```

Put the resulting binary somewhere stable before installing managed jobs. For example:

```bash
mkdir -p ~/.local/bin
mv ./knowledge-sync ~/.local/bin/knowledge-sync
export PATH="$HOME/.local/bin:$PATH"
```

Check the environment before creating a profile:

```bash
knowledge-sync doctor
```

## Quick start

Assume a local source at `~/ExampleVault/Main` and an rclone Drive remote named `example-drive-primary`.

Create a profile:

```bash
knowledge-sync profile add notes-main \
  ~/ExampleVault/Main \
  example-drive-primary \
  "Knowledge Mirror/Notes"
```

`profile add` creates the managed remote root and ownership sidecar, stores the profile, ensures the global worker is available, and queues the initial sync. It returns without waiting for the transfer by default.

Watch initial synchronization until the profile becomes ready:

```bash
knowledge-sync profile status notes-main --watch
```

Or inspect it once:

```bash
knowledge-sync profile status notes-main
```

Install the per-profile watcher and scheduled reconciliation jobs:

```bash
knowledge-sync install notes-main
```

Request a reconciliation at any time:

```bash
knowledge-sync sync notes-main
```

The CLI records the request and wakes the worker; the CLI process does not perform a competing foreground transfer.

## Profiles

Profiles bind one local source directory to one managed remote root.

```text
knowledge-sync profile add <id> <source> <remote> <remote-path>
```

Useful profile commands:

```bash
knowledge-sync profile list
knowledge-sync profile show notes-main
knowledge-sync profile status notes-main
knowledge-sync profile disable notes-main
knowledge-sync profile enable notes-main
knowledge-sync profile remove notes-main
```

Profile types are `generic` and `obsidian`:

```bash
knowledge-sync profile add notes-main ~/ExampleVault/Main \
  example-drive-primary "Knowledge Mirror/Notes" \
  --type obsidian
```

Important `profile add` controls include:

- `--max-delete N` — per-profile deletion budget; the application default is 100 when no override is supplied.
- `--max-file-size N` — maximum managed file size in bytes; the default is 512 MiB.
- `--dry-run` — validate without creating remote or durable profile state.
- `--wait` — wait for the initial worker-owned sync to reach ready.

## Ignore policy and pruning

Ignore rules use `.gitignore` semantics. The synchronization policy is a **committed snapshot** rather than whatever happens to be on disk at transfer time.

After changing `.gitignore` files under a source tree, inspect and commit the new policy:

```bash
knowledge-sync profile ignore status notes-main
knowledge-sync profile ignore update notes-main
```

A policy update can make previously managed remote objects become suppressed. Those objects are not silently deleted. Pruning is an explicit two-step operation.

First freeze the current suppressed set into an immutable request:

```bash
knowledge-sync profile prune preview notes-main
```

Then authorize that exact request:

```bash
knowledge-sync profile prune execute <request-id>
```

If the frozen candidate set exceeds the profile's normal deletion budget, provide an explicit one-request ceiling:

```bash
knowledge-sync profile prune execute <request-id> --allow-deletes 250
```

Track progress with:

```bash
knowledge-sync profile prune status notes-main
# or
knowledge-sync profile prune status <request-id>
```

## Compiler and derived corpus

`knowledge-sync` can compile the eligible local corpus for a profile into an immutable local generation. Compilation is local; the worker owns publication of the desired derived generation to the remote.

```bash
knowledge-sync compile notes-main
knowledge-sync compiler status notes-main
```

Wait for remote derived publication when needed:

```bash
knowledge-sync compile notes-main --wait
```

Verify the local generation's stored artifact hashes:

```bash
knowledge-sync compiler status notes-main --verify
```

Remove local compiler artifacts and queue derived cleanup:

```bash
knowledge-sync compiler clean notes-main
```

## Verification and recovery tools

Check that a remote mirror matches its local source:

```bash
knowledge-sync verify notes-main
```

Run the more expensive full audit:

```bash
knowledge-sync verify notes-main --full
```

Inspect duplicate remote object names:

```bash
knowledge-sync repair-duplicates notes-main
```

The command reports duplicates but deliberately does not auto-run a destructive dedupe.

Stop managed synchronization jobs without touching remote data:

```bash
knowledge-sync stop
```

Remove installed launchd jobs:

```bash
knowledge-sync uninstall notes-main
# or all profile jobs plus the global worker
knowledge-sync uninstall --all
```

Remote purge is intentionally explicit and destructive:

```bash
knowledge-sync purge-remote notes-main --confirm
```

Ownership is validated before the managed remote root is deleted, and Google Drive Trash is not emptied automatically.

## Runtime model

The system separates control-plane commands from data-plane execution:

1. CLI commands update durable intent/state in SQLite.
2. A single global worker claims and executes synchronization work.
3. Per-profile watcher jobs wake work in response to local changes.
4. Scheduled reconciliation provides a periodic convergence path.
5. Status reads live worker telemetry when available and falls back to durable SQLite state otherwise.

This avoids multiple CLI processes starting competing transfers for the same profile.

## Local data

`knowledge-sync` owns the following locations:

| Purpose | Path |
| --- | --- |
| Application config | `~/.config/knowledge-sync/config.json` |
| SQLite state | `~/.local/share/knowledge-sync/knowledge-sync.sqlite` |
| State backups | `~/.local/share/knowledge-sync/backups/` |
| Compiler generations | `~/.local/share/knowledge-sync/compiler/` |
| Logs | `~/Library/Logs/knowledge-sync/` |
| Managed jobs | `~/Library/LaunchAgents/` |

The optional application config controls additional safe rclone arguments. Flags owned by `knowledge-sync`—such as filters, config selection, deletion limits, dry-run behavior, and transfer file lists—cannot be overridden there.

## Worker socket

Status clients normally use the default worker Unix socket path. An override can be managed with:

```bash
knowledge-sync config get socket-path
knowledge-sync config set socket-path /ABSOLUTE/PATH/TO/knowledge-sync.sock
knowledge-sync config unset socket-path
```

Changing the setting restarts the managed worker when one is installed.

## Development

Run the same core checks used by CI:

```bash
go mod verify
go vet ./...
go test -count=1 ./...
go build -trimpath -o knowledge-sync ./cmd/knowledge-sync
```

CI runs the full suite on both Ubuntu and macOS. Runtime integration with `launchd` is macOS-specific.

## Command reference

The CLI is built with Cobra, so every command exposes contextual help:

```bash
knowledge-sync --help
knowledge-sync profile --help
knowledge-sync profile prune --help
knowledge-sync compiler --help
```

Top-level commands currently include profile management, synchronization/reconciliation, background job installation, verification and repair tools, compiler operations, worker/watch internals, diagnostics, and persisted worker configuration.

## Security notes

- Keep the rclone configuration private; it may contain OAuth credentials.
- Do not commit real remote names, account identifiers, folder IDs, local machine paths, logs, databases, or credentials when contributing examples or tests.
- Treat destructive remote operations separately from normal sync. `knowledge-sync` uses ownership checks and explicit authorization gates, but review the target and deletion budget before approving removals.
