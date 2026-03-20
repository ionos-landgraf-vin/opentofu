// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package embed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StateStore is implemented by callers that want to persist Terraform state
// outside the local filesystem.  Only context.Context and []byte are used —
// no OpenTofu types leak through the interface.
type StateStore interface {
	// LoadState returns raw terraform.tfstate bytes, or nil for a first run.
	LoadState(ctx context.Context) ([]byte, error)
	// SaveState receives the full terraform.tfstate bytes after Apply/Destroy.
	// Called with nil after a complete Destroy that removes all resources.
	SaveState(ctx context.Context, state []byte) error
}

// Workspace holds a directory of Terraform configuration files and an optional
// StateStore.  Use the constructors below to create a Workspace.
type Workspace struct {
	dir     string // resolved directory path
	tempDir string // non-empty iff this Workspace owns the dir

	// StateStore, if non-nil, is consulted before and after each operation.
	// See loadStateToDir / saveStateFromDir for the exact hook points.
	StateStore StateStore
}

// resolvedDir returns the workspace directory path.
func (w *Workspace) resolvedDir() string { return w.dir }

// NewWorkspaceFromDir wraps an existing directory that already contains .tf
// files.  No temporary directory is created; Close is a no-op.
func NewWorkspaceFromDir(dir string) *Workspace {
	return &Workspace{dir: dir}
}

// NewWorkspaceFromString creates a temporary directory and writes config as
// "main.tf".  Call Close (or defer ws.Close()) to remove the temp dir.
func NewWorkspaceFromString(config string) (*Workspace, error) {
	return NewWorkspaceFromFiles(map[string]string{"main.tf": config})
}

// NewWorkspaceFromFiles creates a temporary directory and writes the provided
// files into it.  Keys must be plain filenames with no path components — any
// key containing a path separator or ".." is rejected and the temp dir is
// cleaned up before returning the error.  Call Close (or defer ws.Close()) to
// remove the temp dir.
func NewWorkspaceFromFiles(files map[string]string) (*Workspace, error) {
	dir, err := os.MkdirTemp("", "opentofu-embed-*")
	if err != nil {
		return nil, fmt.Errorf("create temp workspace: %w", err)
	}

	for name, content := range files {
		if strings.ContainsRune(name, os.PathSeparator) || strings.Contains(name, "..") {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("invalid file name %q: must be a plain filename without path components", name)
		}
		dest := filepath.Join(dir, name)
		if err := os.WriteFile(dest, []byte(content), 0600); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("write workspace file %q: %w", name, err)
		}
	}

	return &Workspace{dir: dir, tempDir: dir}, nil
}

// Close removes the temporary directory if one was created by a constructor.
// Safe to call multiple times; subsequent calls are no-ops.
func (w *Workspace) Close() error {
	if w.tempDir == "" {
		return nil
	}
	dir := w.tempDir
	w.tempDir = ""
	return os.RemoveAll(dir)
}

// loadStateToDir writes the state from StateStore into the workspace directory
// so OpenTofu can read it.  No-op when StateStore is nil or returns nil bytes.
func (w *Workspace) loadStateToDir(ctx context.Context) error {
	if w.StateStore == nil {
		return nil
	}
	data, err := w.StateStore.LoadState(ctx)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if data == nil {
		return nil
	}
	dest := filepath.Join(w.dir, "terraform.tfstate")
	if err := os.WriteFile(dest, data, 0600); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	return nil
}

// saveStateFromDir reads the state file written by OpenTofu and passes it to
// StateStore.SaveState.  If the file does not exist (e.g. after a complete
// Destroy), SaveState is called with nil.  No-op when StateStore is nil.
func (w *Workspace) saveStateFromDir(ctx context.Context) error {
	if w.StateStore == nil {
		return nil
	}
	path := filepath.Join(w.dir, "terraform.tfstate")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		data = nil
	} else if err != nil {
		return fmt.Errorf("read state file: %w", err)
	}
	if err := w.StateStore.SaveState(ctx, data); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}
