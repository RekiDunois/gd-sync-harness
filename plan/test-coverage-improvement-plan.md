# Test Coverage Improvement Plan

## Scope

This plan covers the Go packages and the shell-based publication audit in the
repository. The goal is not to maximize a single percentage. The priority is
to cover the state transitions and destructive-operation safety boundaries that
can lose data or leave a profile unrecoverable.

## Baseline

Measured on branch `research/test-coverage-improvement` at the current `master`
tip with a clean checkout.

| Measurement | Command | Result |
| --- | --- | ---: |
| CI-like package-local coverage | `go test -count=1 -coverprofile=coverage.out ./...` | 48.4% |
| Cross-package coverage | `go test -count=1 -coverpkg=./... -coverprofile=coverage-all.out ./...` | 54.9% |

The package-local figure is the current CI-equivalent baseline. The
cross-package figure is the better total-progress metric because CLI and
integration tests exercise lower-level packages. CI should generate and retain
both profiles: use the cross-package profile for the total-coverage ratchet,
and use the package-local profile for safety-critical package floors and the
package-specific phase targets below. This prevents a package from appearing
healthy only because another package happened to call it.

### Package-local snapshot

| Package | Coverage | Assessment |
| --- | ---: | --- |
| `internal/namespace` | 100.0% | Complete small helper |
| `internal/live` | 79.9% | Good; failure/reconnect edges remain |
| `internal/policy` | 86.4% | Good |
| `internal/flock` | 78.4% | Good |
| `internal/sched` | 78.3% | Good |
| `internal/config` | 76.7% | Good; CLI/config edges remain |
| `internal/compiler` | 71.5% | Good; frontmatter and generation edges remain |
| `internal/watch` | 67.7% | Moderate; lifecycle/debounce edges remain |
| `internal/filter` | 67.2% | Moderate |
| `internal/launchd` | 69.6% | Moderate; destructive OS operations are mostly untested |
| `internal/source` | 69.9% | Moderate; overlap and traversal errors remain |
| `internal/exec` | 58.4% | Moderate; backend/config/error helpers remain |
| `internal/state` | 53.1% | High priority: durable state machine is under-tested |
| `internal/sync` | 51.8% | High priority: verification and reconcile branches remain |
| `internal/profile` | 43.4% | High priority: lifecycle validation is partial |
| `internal/derived` | 37.0% | High priority: publish/purge ownership matrix is partial |
| `internal/cli` | 32.6% | High priority, but command constructors should be measured via behavior |
| `internal/remote`, `internal/sidecar`, `internal/paths` | 0.0% | No package-local tests |
| `internal/logger`, `pkg/version`, CLI entrypoint | 0.0% | Thin/entrypoint code; cover selectively |

The zero-percent packages are not all equally risky. `remote` and `sidecar`
contain ownership and remote-target safety checks and must be covered. `paths`
and `logger` are mostly deterministic helpers. The CLI `main` function and
Cobra constructors should not be used as the primary coverage target; command
behavior is more valuable than executing registration lines.

## Findings From Recent PRs

- PR #1 introduced ignored remote-orphan discovery and immutable prune preview
  coverage. The scenario tests cover the intended destructive safety behavior,
  but the lower-level remote and sidecar contracts remain largely untested.
- PR #2 and PR #4 added batch deletion, checkpointing, and empty-directory
  cleanup tests. The prune worker is one of the better-covered safety areas;
  retry/failure persistence in `internal/state` is still incomplete.
- PR #6 added strong parser characterization tests and a compiler CLI end-to-end
  test. This explains the comparatively good compiler coverage, while compiler
  CLI command wiring and derived publication remain mostly unexercised.
- PR #7 fixed a test that depended on the runner's home directory. All new
  tests should continue to use `t.TempDir`, temporary `HOME`, and explicit
  state directories.
- PR #8 hardens `scripts/publication-audit.sh`, but Go coverage does not measure
  it and CI does not currently execute it. It needs an independent shell test
  harness with synthetic fixtures before it can be treated as protected code.

## Prioritized Work

### Phase 0: Make Measurement Repeatable

1. Add a CI coverage job that generates both profiles: the package-local
   `go test -count=1 -coverprofile=coverage-package.out ./...` profile and the
   cross-package `go test -count=1 -coverpkg=./... -coverprofile=coverage-cross.out ./...`
   profile. Publish both `go tool cover -func` summaries as artifacts or job
   summaries.
2. Keep `go test -count=1 ./...` as the correctness gate; make coverage
   informational for the first iteration to avoid setting a misleading gate on
   an uncalibrated metric.
3. Define the cross-package total as the ratchet metric. Define package floors
   and the Phase 2/3 package targets against the package-local profile. A total
   increase must not hide a regression in `state`, `sync`, `remote`, or
   `sidecar`.
4. Run `go vet ./...`, the normal suite, and a periodic `go test -race ./...`
   for concurrency-heavy `state`, `live`, `sched`, and `watch` code.

### Phase 1: Cover Ownership and Durable State Boundaries

This is the highest-value work. Use table-driven tests against one temporary
SQLite database and fake rclone binaries that record arguments and return
controlled stdout, stderr, and exit codes.

1. Add `internal/sidecar` tests for valid ownership, UUID/folder/remote/schema
   mismatches, malformed metadata, missing sidecars, temporary transport errors,
   `Exists`, `DerivedExists`, read/write round trips, and derived-sidecar
   validation.
2. Add `internal/remote` tests for remote-name normalization, backend
   validation, managed-root creation, nested and duplicate folder resolution,
   eventual-consistency retries, strict binding failures, not-found versus
   transport errors in `InspectPath`, and quota OK/low/full/unknown outcomes.
   Before testing the retry path, make the folder resolver's wait injectable at
   the package boundary (for example, a package-private `Manager` sleeper or
   retry policy). Production uses the real sleep; tests use a no-op recorder and
   assert the retry count and the exact 200ms, 400ms, 800ms, and 1600ms backoff
   sequence without waiting.
3. Add `internal/state` tests for the untested durable transitions:
   `FailCompilerRun`, `FinishCompilerClean`, `FinishDerivedRunFailure`,
   `UpdateDerivedRunPhase`, `ScheduleDestructiveReconcile`,
   `PromoteToFullReconcile`, `OrphanRuns`, settings, runtime markers, and
   profile enable/disable/deletion lifecycle.
4. Test the retry matrix explicitly: ordinary retryable, limited retryable,
   terminal, worker interruption, backoff deadline, manual-delete override
   consumption, and retry after a newer desired generation.
5. Test transaction invariants: failed operations do not partially advance the
   state, success is bound to the claimed run/target, newer events survive a
   clear, and suppressed manifest rows survive refresh.

**Exit target:** cross-package coverage at least 62%, with no zero-coverage
function in `remote` or `sidecar` unless it is deliberately documented as an
OS/process boundary.

### Phase 2: Cover Sync and Worker Failure Behavior

1. Add focused `internal/sync` tests for `VerifyCheck`, `VerifyFull`, malformed
   and empty files-from lists, rclone failures, malformed dry-run summaries,
   delete-target rejection, source policy failures, and option propagation.
2. Expand reconciler tests for duplicate remote detection, unstable source,
   delete-budget rejection, zero versus non-zero proven deletes, post-sync dirty
   detection, progress callbacks, and failure before each phase boundary.
3. Add worker tests for ownership failure, profile lock contention, remote lease
   contention, claim results, derived publish/purge success and failure,
   interrupted run recovery, cleanup after compiler clean, and structured error
   classification by rclone exit code.
4. Replace remaining scheduler/reconcile sleeps in new tests with deterministic
   seams where practical. If a clock or debounce dependency must be introduced,
   keep it local to that boundary rather than making time global mutable state.

**Exit target:** `internal/state` and `internal/sync` package-local coverage at
least 70%; cross-package total at least 68%.

### Phase 3: Test CLI Behavior, Not Registration Lines

1. Exercise commands through `NewRootCmd`/`SetArgs` with temporary `HOME` and
   state directories: profile lifecycle, config get/set/unset, reconcile,
   verify, prune preview/execute/status, ignore update/status, compiler status,
   and worker `--once`.
2. Assert durable database effects and user-visible errors, including missing
   profile, tombstoned/disabled profile, stale policy, ownership failure,
   missing worker, and invalid arguments.
3. Keep command help smoke tests, but do not pursue artificial coverage for
   Cobra variable declarations or the process-only `main` wrapper.
4. Add focused tests for status rendering and time/byte formatting edge cases;
   these are cheap and protect operator-facing diagnostics.

**Exit target:** `internal/cli` package-local coverage at least 55%, with the
   worker/reconcile/status paths covered by behavior tests rather than only
   helper tests.

### Phase 4: Complete Safety Nets and Set Gates

1. Add tests for `internal/paths`, `internal/logger`, and version formatting,
   using temporary `HOME` and synthetic paths. Keep these tests small and avoid
   treating path construction as more important than state correctness.
2. Add a POSIX shell test for `scripts/publication-audit.sh` covering clean
   trees, current-tree violations, history violations, denylist matches, and
   warning-only/manual-review output with exit code 3. Fixtures must contain
   only synthetic tokens and paths.
3. After two stable CI iterations, enforce a cross-package total threshold of
   65%, then raise it to 70% after Phase 2. Using the package-local profile, add
   package floors of 60% for `internal/state`, `internal/sync`,
   `internal/remote`, and `internal/sidecar`.
4. Use a ratchet rule: new or changed production files must not reduce total
   coverage, and safety-critical packages must not regress below their floor.

## Test Design Rules

- Prefer deterministic unit tests for state transitions and fake rclone command
  scripts for command construction/error mapping.
- Use `t.TempDir()` and temporary `HOME`; never read or write a developer's
  actual config, home directory, launchd state, or cloud account.
- Keep real-rclone integration tests as a separate, clearly named layer. They
  should skip only when rclone is unavailable and must not be required for the
  deterministic state-machine suite.
- Assert both positive behavior and fail-closed behavior. For destructive
  operations, verify the exact command arguments and that no command is issued
  after a rejected precondition.
- Use `-count=1` for coverage measurement and run the race detector separately;
  coverage numbers from race runs are not the release metric.
- Treat coverage as a prioritization signal. A tested line is not evidence that
  all state-machine transitions or safety invariants are tested.

## Definition of Done

The plan is complete when:

- CI publishes a reproducible cross-package coverage report.
- `remote` and `sidecar` have deterministic contract tests.
- State retry, ownership, claim, commit, and recovery transitions have explicit
  success and failure tests.
- Sync and worker tests cover every destructive-operation refusal path.
- The shell publication audit has synthetic regression fixtures.
- Coverage gates use documented total and package floors with a ratchet rule.
- `go vet ./...`, the full test suite, and the scheduled race suite pass without
  relying on real user or deployment data.
