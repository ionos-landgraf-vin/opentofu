// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package embed provides a thin public wrapper around OpenTofu's internal
// command engine, allowing callers to run Init, Plan, Apply, and Destroy
// operations in-process without spawning a child process.
package embed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/mitchellh/cli"
	"github.com/opentofu/svchost/disco"

	backendInit "github.com/opentofu/opentofu/internal/backend/init"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/command"
	"github.com/opentofu/opentofu/internal/command/views"
	"github.com/opentofu/opentofu/internal/command/workdir"
	"github.com/opentofu/opentofu/internal/getproviders"
	"github.com/opentofu/opentofu/internal/terminal"
)

var backendInitOnce sync.Once

// UnmanagedProvider describes a provider that is already running as an
// in-process gRPC server. The caller is responsible for starting the server
// (e.g. using go-plugin's ServeConfig with Test mode) and passing the
// resulting ReattachConfig here. OpenTofu will connect to it without managing
// its lifecycle, so no binary download or network access is required.
type UnmanagedProvider struct {
	// Source is the provider address, e.g. "ionos-cloud/ionoscloud" or the
	// fully-qualified form "registry.terraform.io/ionos-cloud/ionoscloud".
	Source string
	// Reattach is the reattach config returned by the in-process provider
	// server (from github.com/hashicorp/go-plugin).
	Reattach *goplugin.ReattachConfig
}

// RunnerConfig holds configuration for the embedded OpenTofu runner.
type RunnerConfig struct {
	PluginCacheDir   string
	GlobalPluginDirs []string
	// UnmanagedProviders lists providers already running in-process.
	// OpenTofu connects to them directly without downloading binaries.
	UnmanagedProviders []UnmanagedProvider
}

// PlanResult summarizes a completed plan operation.
type PlanResult struct {
	HasChanges bool
	Add        int
	Change     int
	Destroy    int
	Resources  []ResourceChange
}

// ResourceChange describes a single planned resource change.
type ResourceChange struct {
	Addr   string
	Action string
}

// ProgressEvent is emitted during Apply or Destroy operations.
type ProgressEvent struct {
	Addr   string
	Action string
	Done   bool
	Err    error
}

// EmbeddedRunner runs OpenTofu operations in-process.
type EmbeddedRunner struct {
	cfg RunnerConfig
}

// New creates an EmbeddedRunner and initializes the OpenTofu backend registry.
// It is safe to call New multiple times; backend initialization happens exactly once.
func New(cfg RunnerConfig) (*EmbeddedRunner, error) {
	backendInitOnce.Do(func() {
		backendInit.Init(disco.New())
	})
	return &EmbeddedRunner{cfg: cfg}, nil
}

// Init runs `tofu init` in the given workspace.
func (r *EmbeddedRunner) Init(ctx context.Context, ws *Workspace) error {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return fmt.Errorf("create stderr pipe: %w", err)
	}
	// Drain pipes to prevent blocking; output is discarded for init.
	var drainWg sync.WaitGroup
	drainWg.Add(2)
	go func() { defer drainWg.Done(); _, _ = io.Copy(io.Discard, stdoutR) }()
	go func() { defer drainWg.Done(); _, _ = io.Copy(io.Discard, stderrR) }()

	meta := r.buildMeta(ctx, ws.resolvedDir(), stdoutW, stderrW)
	cmd := &command.InitCommand{Meta: meta}
	code := cmd.Run([]string{"-no-color", "-input=false"})

	_ = stdoutW.Close()
	_ = stderrW.Close()
	drainWg.Wait()
	_ = stdoutR.Close()
	_ = stderrR.Close()

	if code != 0 {
		return fmt.Errorf("tofu init failed (exit %d)", code)
	}
	return nil
}

// Plan runs `tofu plan -json` in the given workspace and returns a summary of
// the planned changes.  vars are passed as -var flags.
func (r *EmbeddedRunner) Plan(ctx context.Context, ws *Workspace, vars map[string]string) (*PlanResult, error) {
	if err := ws.loadStateToDir(ctx); err != nil {
		return nil, err
	}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("create stderr pipe: %w", err)
	}

	// Capture stdout for JSON parsing; discard stderr.
	var stdoutBuf bytes.Buffer
	var captureWg sync.WaitGroup
	captureWg.Add(1)
	go func() {
		defer captureWg.Done()
		_, _ = io.Copy(&stdoutBuf, stdoutR)
	}()
	go func() { _, _ = io.Copy(io.Discard, stderrR) }()

	meta := r.buildMeta(ctx, ws.resolvedDir(), stdoutW, stderrW)
	cmd := &command.PlanCommand{Meta: meta}

	args := []string{"-json", "-input=false", "-detailed-exitcode", "-no-color"}
	for k, v := range vars {
		args = append(args, "-var", k+"="+v)
	}
	code := cmd.Run(args)

	_ = stdoutW.Close()
	_ = stderrW.Close()
	captureWg.Wait()
	_ = stdoutR.Close()
	_ = stderrR.Close()

	switch code {
	case 0:
		return &PlanResult{HasChanges: false}, nil
	case 2:
		result := parsePlanJSON(stdoutBuf.Bytes())
		result.HasChanges = true
		return result, nil
	default:
		return nil, extractPlanError(stdoutBuf.Bytes())
	}
}

// Apply runs `tofu apply -json -auto-approve` in the given workspace.
// onProgress is called after each apply_start / apply_complete / apply_errored event.
func (r *EmbeddedRunner) Apply(ctx context.Context, ws *Workspace, vars map[string]string, onProgress func(ProgressEvent)) error {
	return r.runApplyOrDestroy(ctx, ws, vars, false, onProgress)
}

// Destroy runs `tofu destroy -json -auto-approve` in the given workspace.
// onProgress is called after each apply_start / apply_complete / apply_errored event.
func (r *EmbeddedRunner) Destroy(ctx context.Context, ws *Workspace, vars map[string]string, onProgress func(ProgressEvent)) error {
	return r.runApplyOrDestroy(ctx, ws, vars, true, onProgress)
}

func (r *EmbeddedRunner) runApplyOrDestroy(ctx context.Context, ws *Workspace, vars map[string]string, destroy bool, onProgress func(ProgressEvent)) error {
	if err := ws.loadStateToDir(ctx); err != nil {
		return err
	}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return fmt.Errorf("create stderr pipe: %w", err)
	}

	// Capture stdout for JSON line-by-line progress streaming.
	var capturedErr *ApplyError
	var captureWg sync.WaitGroup
	captureWg.Add(1)
	go func() {
		defer captureWg.Done()
		capturedErr = streamApplyJSON(stdoutR, onProgress)
	}()
	go func() { _, _ = io.Copy(io.Discard, stderrR) }()

	meta := r.buildMeta(ctx, ws.resolvedDir(), stdoutW, stderrW)

	verb := "apply"
	if destroy {
		verb = "destroy"
	}

	args := []string{"-json", "-input=false", "-auto-approve", "-no-color"}
	for k, v := range vars {
		args = append(args, "-var", k+"="+v)
	}
	cmd := &command.ApplyCommand{Meta: meta, Destroy: destroy}
	code := cmd.Run(args)

	_ = stdoutW.Close()
	_ = stderrW.Close()
	captureWg.Wait()
	_ = stdoutR.Close()
	_ = stderrR.Close()

	if code != 0 {
		if capturedErr != nil {
			return capturedErr
		}
		return fmt.Errorf("tofu %s failed (exit %d)", verb, code)
	}
	if err := ws.saveStateFromDir(ctx); err != nil {
		return err
	}
	return nil
}

// buildMeta constructs a command.Meta for a single operation.
func (r *EmbeddedRunner) buildMeta(ctx context.Context, workspaceDir string, stdout, stderr *os.File) command.Meta {
	stdinNull, _ := os.Open(os.DevNull)

	streams := &terminal.Streams{
		Stdout: &terminal.OutputStream{File: stdout},
		Stderr: &terminal.OutputStream{File: stderr},
		Stdin:  &terminal.InputStream{File: stdinNull},
	}
	view := views.NewView(streams).SetRunningInAutomation(true)

	services := disco.New()

	// Wire up in-process providers. OpenTofu connects to them directly without
	// managing their lifecycle, so no binary download is required.
	unmanagedProviders := make(map[addrs.Provider]*goplugin.ReattachConfig)
	for _, up := range r.cfg.UnmanagedProviders {
		addr, diags := addrs.ParseProviderSourceString(up.Source)
		if diags.HasErrors() {
			continue // invalid source strings are surfaced at init time
		}
		unmanagedProviders[addr] = up.Reattach
	}

	return command.Meta{
		WorkingDir:          workdir.NewDir(workspaceDir),
		Streams:             streams,
		View:                view,
		Color:               false,
		GlobalPluginDirs:    r.cfg.GlobalPluginDirs,
		Ui:                  &cli.BasicUi{Writer: io.Discard, ErrorWriter: io.Discard},
		Services:            services,
		RunningInAutomation: true,
		PluginCacheDir:      r.cfg.PluginCacheDir,
		CallerContext:       ctx,
		ShutdownCh:          ctx.Done(),
		ProviderSource:      getproviders.NewRegistrySource(ctx, services, nil, getproviders.LocationConfig{}),
		UnmanagedProviders:  unmanagedProviders,
	}
}

// --- JSON parsing helpers ---

// tfLine is the common envelope for OpenTofu's machine-readable JSON output.
type tfLine struct {
	Level   string `json:"@level"`
	Type    string `json:"type"`
	Message string `json:"@message"`

	Changes *struct {
		Add    int `json:"add"`
		Change int `json:"change"`
		Remove int `json:"remove"`
	} `json:"changes,omitempty"`

	Diagnostic *struct {
		Severity string `json:"severity"`
		Summary  string `json:"summary"`
		Detail   string `json:"detail"`
		Address  string `json:"address"`
	} `json:"diagnostic,omitempty"`

	Change *struct {
		Resource *struct{ Addr string `json:"addr"` } `json:"resource"`
		Action   string                               `json:"action"`
	} `json:"change,omitempty"`

	Hook *struct {
		Resource       struct{ Addr string `json:"addr"` } `json:"resource"`
		Action         string                              `json:"action"`
		ElapsedSeconds float64                             `json:"elapsed_seconds"`
	} `json:"hook,omitempty"`
}

func parsePlanJSON(output []byte) *PlanResult {
	result := &PlanResult{}
	scanner := newScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var line tfLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type == "change_summary" && line.Changes != nil {
			result.Add = line.Changes.Add
			result.Change = line.Changes.Change
			result.Destroy = line.Changes.Remove
		}
		if line.Type == "planned_change" && line.Change != nil && line.Change.Resource != nil {
			result.Resources = append(result.Resources, ResourceChange{
				Addr:   line.Change.Resource.Addr,
				Action: line.Change.Action,
			})
		}
	}
	return result
}

func extractPlanError(output []byte) error {
	scanner := newScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var line tfLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Level == "error" && line.Diagnostic != nil {
			msg := line.Diagnostic.Summary
			if line.Diagnostic.Detail != "" {
				msg += "\n" + line.Diagnostic.Detail
			}
			return &ApplyError{resource: line.Diagnostic.Address, message: msg}
		}
	}
	return fmt.Errorf("tofu plan failed")
}

// ApplyError is returned when an OpenTofu apply/destroy/plan diagnostic error
// is captured. It carries the optional resource address and message text.
type ApplyError struct {
	resource string
	message  string
}

func (e *ApplyError) Error() string {
	if e.resource != "" {
		return fmt.Sprintf("tofu error on %s: %s", e.resource, e.message)
	}
	return "tofu error: " + e.message
}

// Resource returns the resource address, if any.
func (e *ApplyError) Resource() string { return e.resource }

// Message returns the error message.
func (e *ApplyError) Message() string { return e.message }

// streamApplyJSON reads JSON lines from r, calls onProgress for each hook
// event, and returns the first diagnostic error encountered (if any).
func streamApplyJSON(r io.Reader, onProgress func(ProgressEvent)) *ApplyError {
	var captured *ApplyError
	scanner := newScanner(r)
	for scanner.Scan() {
		var line tfLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		switch line.Type {
		case "apply_start":
			if onProgress != nil && line.Hook != nil {
				onProgress(ProgressEvent{
					Addr:   line.Hook.Resource.Addr,
					Action: line.Hook.Action,
					Done:   false,
				})
			}
		case "apply_complete":
			if onProgress != nil && line.Hook != nil {
				onProgress(ProgressEvent{
					Addr:   line.Hook.Resource.Addr,
					Action: line.Hook.Action,
					Done:   true,
				})
			}
		case "apply_errored":
			if onProgress != nil && line.Hook != nil {
				onProgress(ProgressEvent{
					Addr:   line.Hook.Resource.Addr,
					Action: line.Hook.Action,
					Done:   true,
					Err:    fmt.Errorf("apply errored for %s", line.Hook.Resource.Addr),
				})
			}
		case "diagnostic":
			if line.Level == "error" && line.Diagnostic != nil && captured == nil {
				msg := line.Diagnostic.Summary
				if line.Diagnostic.Detail != "" {
					msg += "\n" + line.Diagnostic.Detail
				}
				captured = &ApplyError{
					resource: line.Diagnostic.Address,
					message:  msg,
				}
			}
		}
	}
	return captured
}

func newScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	return scanner
}
