// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package embed

import (
	"context"
	"sync"
	"testing"
)

// memStateStore is an in-memory StateStore for testing.
type memStateStore struct {
	mu          sync.Mutex
	data        []byte
	loadCalls   int
	saveCalls   int
	savedStates [][]byte
}

func (m *memStateStore) LoadState(_ context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadCalls++
	return m.data, nil
}

func (m *memStateStore) SaveState(_ context.Context, state []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveCalls++
	m.savedStates = append(m.savedStates, state)
	m.data = state
	return nil
}

// TestInMemoryStateStore exercises the full Init → Plan → Apply → Plan → Destroy
// lifecycle using the built-in terraform_data resource (no provider binary or
// network access required) and an in-memory StateStore.
func TestInMemoryStateStore(t *testing.T) {
	const tfConfig = `
resource "terraform_data" "example" {
  input = "hello-embed"
}
`
	ctx := context.Background()

	runner, err := New(RunnerConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ws, err := NewWorkspaceFromString(tfConfig)
	if err != nil {
		t.Fatalf("NewWorkspaceFromString: %v", err)
	}
	// Register ws.Close before t.Chdir so that cleanup order (LIFO) is:
	// 1. restore CWD  2. remove temp workspace dir.
	t.Cleanup(func() { _ = ws.Close() })

	// OpenTofu resolves ConfigDir="." against the process CWD, so we must
	// chdir into the workspace before running any commands.
	t.Chdir(ws.resolvedDir())

	store := &memStateStore{}
	ws.StateStore = store

	// Init — state hooks are NOT called during init.
	if err := runner.Init(ctx, ws); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if store.loadCalls != 0 || store.saveCalls != 0 {
		t.Errorf("after Init: want loadCalls=0 saveCalls=0, got loadCalls=%d saveCalls=%d",
			store.loadCalls, store.saveCalls)
	}

	// Plan — LoadState called; SaveState NOT called (plan is read-only).
	plan, err := runner.Plan(ctx, ws, nil)
	if err != nil {
		t.Fatalf("Plan (first): %v", err)
	}
	if !plan.HasChanges {
		t.Errorf("Plan (first): expected HasChanges=true, got false")
	}
	if plan.Add != 1 {
		t.Errorf("Plan (first): expected Add=1, got %d", plan.Add)
	}
	if store.loadCalls != 1 || store.saveCalls != 0 {
		t.Errorf("after first Plan: want loadCalls=1 saveCalls=0, got loadCalls=%d saveCalls=%d",
			store.loadCalls, store.saveCalls)
	}

	// Apply — LoadState called; SaveState called once with non-nil bytes.
	if err := runner.Apply(ctx, ws, nil, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if store.loadCalls != 2 || store.saveCalls != 1 {
		t.Errorf("after Apply: want loadCalls=2 saveCalls=1, got loadCalls=%d saveCalls=%d",
			store.loadCalls, store.saveCalls)
	}
	if store.savedStates[0] == nil {
		t.Errorf("Apply: expected non-nil state bytes after apply")
	}

	// Plan again — the state loaded from the store must reflect what was
	// applied, so no changes should be planned.  This is the critical assertion:
	// it proves the state round-trip through the store is actually used.
	plan, err = runner.Plan(ctx, ws, nil)
	if err != nil {
		t.Fatalf("Plan (second): %v", err)
	}
	if plan.HasChanges {
		t.Errorf("Plan (second): expected HasChanges=false after apply, got true")
	}
	if store.loadCalls != 3 {
		t.Errorf("after second Plan: want loadCalls=3, got %d", store.loadCalls)
	}

	// Destroy — LoadState called; SaveState called with nil or empty state
	// (a complete destroy leaves no managed resources).
	if err := runner.Destroy(ctx, ws, nil, nil); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if store.loadCalls != 4 || store.saveCalls != 2 {
		t.Errorf("after Destroy: want loadCalls=4 saveCalls=2, got loadCalls=%d saveCalls=%d",
			store.loadCalls, store.saveCalls)
	}
	// After a complete destroy the state is either nil (file removed) or
	// contains an empty-resources JSON blob — either way it must not contain
	// any resource records.
	destroyState := store.savedStates[1]
	if len(destroyState) > 0 {
		// State file still present — verify it has no resources.
		stateStr := string(destroyState)
		if contains(stateStr, `"terraform_data"`) {
			t.Errorf("Destroy: state still contains terraform_data resource after destroy")
		}
	}
}

// contains is a simple substring check used to avoid importing strings in test.
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
