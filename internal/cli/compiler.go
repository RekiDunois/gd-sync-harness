package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	knowledgecompiler "knowledge-sync/internal/compiler"
	"knowledge-sync/internal/flock"
	"knowledge-sync/internal/paths"
	"knowledge-sync/internal/source"
	"knowledge-sync/internal/state"
)

func newCompileCmd() *cobra.Command {
	var wait bool
	c := &cobra.Command{
		Use:   "compile <profile>",
		Short: "Compile the eligible local corpus into an immutable generation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewLocalApp()
			if err != nil {
				return err
			}
			defer app.Close()
			p, err := app.requireProfile(args[0])
			if err != nil {
				return err
			}
			if p.DeletionRequestedAt != nil {
				return fmt.Errorf("profile %q is deletion-requested", p.ID)
			}
			stateRoot, err := paths.StateDir()
			if err != nil {
				return err
			}
			resolvedSource, err := source.ValidateSourceRoot(p.SourcePath, stateRoot)
			if err != nil {
				return fmt.Errorf("source validation: %w", err)
			}
			profiles, err := app.DB.ListProfiles()
			if err != nil {
				return err
			}
			var otherRoots []string
			for _, other := range profiles {
				if other.ID != p.ID {
					otherRoots = append(otherRoots, other.SourcePath)
				}
			}
			if err := source.ValidateSourceOverlap(resolvedSource, otherRoots); err != nil {
				return err
			}
			bundle, err := app.DB.GetCommittedPolicyBundle(p.ID)
			if err != nil {
				return err
			}
			root, err := paths.CompilerRoot(p.ProfileUUID)
			if err != nil {
				return err
			}
			contractHash := source.EligibilityContractHash(bundle.Policy.PolicyHash, p.MaxFileSize)
			runID, generationID := uuid.NewString(), uuid.NewString()
			if err := app.withCompilerLock(p.ProfileUUID, func() error {
				if err := app.DB.StartCompilerRun(p.ID, runID, generationID, knowledgecompiler.CompilerVersion, knowledgecompiler.SchemaVersion, bundle.Policy.PolicyHash, contractHash); err != nil {
					return err
				}
				result, err := knowledgecompiler.Compile(knowledgecompiler.Input{
					ProfileID: p.ID, ProfileUUID: p.ProfileUUID, SourceRoot: p.SourcePath,
					MaxFileSize: p.MaxFileSize, Policy: bundle.Snapshot, PolicyHash: bundle.Policy.PolicyHash,
					CompilerRunID: runID, GenerationID: generationID,
				})
				if err != nil {
					_ = app.DB.FailCompilerRun(p.ID, runID, err.Error())
					return err
				}
				protected := ""
				if compilerState, stateErr := app.DB.GetCompilerState(p.ID); stateErr == nil && compilerState.ActivePublishGenerationID != nil {
					protected = *compilerState.ActivePublishGenerationID
				}
				if _, err := knowledgecompiler.NewStore(root).PublishWithProtected(result, protected); err != nil {
					_ = app.DB.FailCompilerRun(p.ID, runID, err.Error())
					return err
				}
				binding := state.DerivedBindingFingerprint(p.RemoteName, p.RemoteFolderID)
				if err := app.DB.FinishCompilerRun(p.ID, runID, generationID, result.Manifest.SourceSnapshotID, bundle.Policy.PolicyHash, contractHash, binding, result.Manifest.Counts.EligibleFiles, result.Manifest.Counts.Warnings); err != nil {
					return fmt.Errorf("local_published_state_pending_repair: %w", err)
				}
				return nil
			}); err != nil {
				return err
			}
			fmt.Printf("Compiled generation %s locally.\n", generationID)
			fmt.Printf("Local compiler root: %s\n", root)
			wakeWorker(app, p.ID)
			if wait {
				if err := waitForDerived(app, p, generationID); err != nil {
					return err
				}
				fmt.Printf("Derived generation %s published.\n", generationID)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&wait, "wait", false, "wait for remote derived publication")
	return c
}

func newCompilerCmd() *cobra.Command {
	c := &cobra.Command{Use: "compiler", Short: "Inspect and manage local compiler generations"}
	c.AddCommand(newCompilerStatusCmd(), newCompilerCleanCmd())
	return c
}

func newCompilerStatusCmd() *cobra.Command {
	var verify bool
	c := &cobra.Command{
		Use:   "status <profile>",
		Short: "Show local compiler and derived operational state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewLocalApp()
			if err != nil {
				return err
			}
			defer app.Close()
			p, err := app.DB.GetProfile(args[0])
			if err != nil {
				return err
			}
			root, err := paths.CompilerRoot(p.ProfileUUID)
			if err != nil {
				return err
			}
			store := knowledgecompiler.NewStore(root)
			pointer, pointerErr := store.Current()
			compilerState, stateErr := app.DB.GetCompilerState(p.ID)
			if stateErr != nil && !errors.Is(stateErr, state.ErrNotFound) {
				return stateErr
			}
			fmt.Printf("profile: %s\n", p.ID)
			fmt.Printf("local root: %s\n", root)
			if pointerErr == nil {
				fmt.Printf("local generation: %s\n", pointer.CurrentGenerationID)
				if verify {
					if _, err := store.VerifyCurrent(); err != nil {
						return fmt.Errorf("local integrity: %w", err)
					}
					fmt.Println("local integrity: verified")
				}
			} else if os.IsNotExist(pointerErr) {
				fmt.Println("local generation: absent")
			} else {
				return pointerErr
			}
			if compilerState != nil {
				fmt.Printf("derived desired: %s", compilerState.DesiredDerivedMode)
				if compilerState.DesiredDerivedGenerationID != nil {
					fmt.Printf(" (%s)", *compilerState.DesiredDerivedGenerationID)
				}
				fmt.Println()
				fmt.Printf("derived state: %s\n", compilerState.DerivedState)
				fmt.Printf("remote published: %s\n", nullable(compilerState.RemotePublishedGenerationID))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&verify, "verify", false, "verify every local artifact hash")
	return c
}

func newCompilerCleanCmd() *cobra.Command {
	var wait bool
	c := &cobra.Command{
		Use:   "clean <profile>",
		Short: "Remove local compiler artifacts and queue derived cleanup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewLocalApp()
			if err != nil {
				return err
			}
			defer app.Close()
			p, err := app.requireProfile(args[0])
			if err != nil {
				return err
			}
			if p.DeletionRequestedAt != nil {
				return fmt.Errorf("profile %q is deletion-requested", p.ID)
			}
			root, err := paths.CompilerRoot(p.ProfileUUID)
			if err != nil {
				return err
			}
			operationID := uuid.NewString()
			var protectedGeneration string
			if err := app.withCompilerLock(p.ProfileUUID, func() error {
				if err := app.DB.MarkCompilerClean(p.ID, operationID); err != nil {
					return err
				}
				compilerState, err := app.DB.GetCompilerState(p.ID)
				if err != nil {
					return err
				}
				if compilerState.ActivePublishGenerationID != nil {
					protectedGeneration = *compilerState.ActivePublishGenerationID
				}
				if err := os.Remove(filepath.Join(root, "MANIFEST.json")); err != nil && !os.IsNotExist(err) {
					return err
				}
				entries, err := os.ReadDir(filepath.Join(root, "generations"))
				if err != nil && !os.IsNotExist(err) {
					return err
				}
				for _, entry := range entries {
					if entry.Name() == protectedGeneration {
						continue
					}
					if err := os.RemoveAll(filepath.Join(root, "generations", entry.Name())); err != nil {
						return err
					}
				}
				if err := os.RemoveAll(filepath.Join(root, "staging")); err != nil {
					return err
				}
				if protectedGeneration == "" {
					return app.DB.FinishCompilerClean(p.ID)
				}
				return nil
			}); err != nil {
				return err
			}
			fmt.Println("Compiler artifacts cleaned locally.")
			if protectedGeneration != "" {
				fmt.Printf("Preserved in-flight generation %s until DerivedSync finishes.\n", protectedGeneration)
			}
			wakeWorker(app, p.ID)
			if wait {
				if err := waitForDerived(app, p, ""); err != nil {
					return err
				}
				fmt.Println("Derived namespace purged.")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&wait, "wait", false, "wait for remote derived cleanup")
	return c
}

func waitForDerived(app *App, p *state.Profile, generationID string) error {
	deadline := time.Now().Add(24 * time.Hour)
	binding := state.DerivedBindingFingerprint(p.RemoteName, p.RemoteFolderID)
	for {
		current, err := app.DB.GetCompilerState(p.ID)
		if err != nil {
			return err
		}
		if generationID == "" {
			if current.RemoteState == "absent" && current.RemoteStateBindingFingerprint != nil && *current.RemoteStateBindingFingerprint == binding {
				return nil
			}
		} else if current.RemotePublishedGenerationID != nil && *current.RemotePublishedGenerationID == generationID &&
			current.RemotePublishedBindingFingerprint != nil && *current.RemotePublishedBindingFingerprint == binding {
			return nil
		}
		if current.DerivedState == "failed" && current.DerivedTerminalErrorCode != nil {
			return fmt.Errorf("derived sync blocked by terminal error: %s", *current.DerivedTerminalErrorCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for derived state")
		}
		time.Sleep(5 * time.Second)
	}
}

func (a *App) withCompilerLock(profileUUID string, fn func() error) error {
	lock, err := flock.Acquire(a.LockDir, knowledgecompilerLockName(profileUUID))
	if err != nil {
		return err
	}
	defer lock.Release()
	return fn()
}

func knowledgecompilerLockName(profileUUID string) string {
	return paths.CompilerLockName(profileUUID)
}

func nullable(value *string) string {
	if value == nil {
		return "-"
	}
	return *value
}
