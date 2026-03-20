# IONOS Fork: `pkg/embed`

This fork of OpenTofu adds a single public package, `pkg/embed`, that exposes a thin wrapper
around OpenTofu's internal command engine. It allows Go programs to run Init, Plan, Apply, and
Destroy operations **in-process** — no `opentofu` binary required, no subprocess management.

---

## Why this fork exists

OpenTofu's execution engine lives entirely in `internal/` packages, which Go's module system
prevents from being imported outside the module. This fork adds `pkg/embed` — a minimal public
surface inside the same module — so that Go services can embed OpenTofu directly.

The fork-specific files are:

| File | Purpose |
|---|---|
| `pkg/embed/runner.go` | `EmbeddedRunner`, public types, JSON parsing |
| `pkg/embed/workspace.go` | `Workspace`, `StateStore` interface |
| `pkg/embed/runner_test.go` | Integration test (Init → Plan → Apply → Destroy) |
| `docs/ionos.md` | This document |

All other files are unmodified upstream OpenTofu source.

---

## The `pkg/embed` API

### Workspace

A `Workspace` holds a directory of Terraform configuration files and an optional `StateStore`.

```go
// NewWorkspaceFromDir wraps an existing directory that already contains .tf files.
// Close is a no-op.
func NewWorkspaceFromDir(dir string) *Workspace

// NewWorkspaceFromString creates a temp directory and writes config as "main.tf".
// Call ws.Close() (or defer ws.Close()) to remove the temp dir.
func NewWorkspaceFromString(config string) (*Workspace, error)

// NewWorkspaceFromFiles creates a temp directory and writes the provided files into it.
// Keys must be plain filenames with no path components.
// Call ws.Close() (or defer ws.Close()) to remove the temp dir.
func NewWorkspaceFromFiles(files map[string]string) (*Workspace, error)

// Close removes the temporary directory, if one was created. Safe to call multiple times.
func (w *Workspace) Close() error
```

A `Workspace` also has a `StateStore` field:

```go
type Workspace struct {
    // StateStore, if non-nil, is consulted before and after each operation.
    // LoadState is called before Plan, Apply, and Destroy.
    // SaveState is called after Apply and Destroy.
    StateStore StateStore
}
```

### StateStore

Implement `StateStore` to persist state outside the local filesystem (e.g. in a database or
object store). Both methods receive raw `terraform.tfstate` bytes — no OpenTofu types leak
through.

```go
type StateStore interface {
    // LoadState returns raw terraform.tfstate bytes, or nil for a first run.
    LoadState(ctx context.Context) ([]byte, error)

    // SaveState receives the full terraform.tfstate bytes after Apply/Destroy.
    // Called with nil after a complete Destroy that removes all resources.
    SaveState(ctx context.Context, state []byte) error
}
```

Hook points:

| Operation | LoadState | SaveState |
|---|---|---|
| Init | — | — |
| Plan | called once | — |
| Apply | called once | called once (non-nil bytes) |
| Destroy | called once | called once (nil or empty-state bytes) |

### Runner types

```go
// UnmanagedProvider describes a provider already running as an in-process
// gRPC server. The caller starts the server (via go-plugin's test/reattach
// mechanism) and passes the resulting ReattachConfig here. OpenTofu connects
// to it directly — no binary download or network access required.
type UnmanagedProvider struct {
    Source   string                    // e.g. "ionos-cloud/ionoscloud"
    Reattach *goplugin.ReattachConfig  // from github.com/hashicorp/go-plugin
}

// RunnerConfig holds configuration for the embedded runner.
type RunnerConfig struct {
    PluginCacheDir     string              // equivalent to TF_PLUGIN_CACHE_DIR
    GlobalPluginDirs   []string            // additional provider search paths
    UnmanagedProviders []UnmanagedProvider // in-process providers (no network needed)
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
    Addr   string // e.g. "ionoscloud_server.web"
    Action string // e.g. "create", "update", "delete"
}

// ProgressEvent is emitted during Apply or Destroy operations.
type ProgressEvent struct {
    Addr   string
    Action string
    Done   bool  // false = started, true = completed or errored
    Err    error // non-nil on apply_errored events
}

// ApplyError is returned when OpenTofu emits a diagnostic error during
// plan, apply, or destroy. It carries the resource address and message.
type ApplyError struct { ... }

func (e *ApplyError) Resource() string // resource address (may be empty)
func (e *ApplyError) Message() string  // human-readable error text
func (e *ApplyError) Error() string
```

### Runner functions

```go
// New creates an EmbeddedRunner and initializes the backend registry.
// Safe to call multiple times; initialization happens exactly once.
func New(cfg RunnerConfig) (*EmbeddedRunner, error)

// Init runs `tofu init` in the given workspace.
func (r *EmbeddedRunner) Init(ctx context.Context, ws *Workspace) error

// Plan runs `tofu plan -json` and returns a summary of planned changes.
// vars are passed as -var flags.
func (r *EmbeddedRunner) Plan(ctx context.Context, ws *Workspace, vars map[string]string) (*PlanResult, error)

// Apply runs `tofu apply -json -auto-approve`.
// onProgress is called for each apply_start / apply_complete / apply_errored event.
func (r *EmbeddedRunner) Apply(ctx context.Context, ws *Workspace, vars map[string]string, onProgress func(ProgressEvent)) error

// Destroy runs `tofu destroy -json -auto-approve`.
// onProgress is called for each apply_start / apply_complete / apply_errored event.
func (r *EmbeddedRunner) Destroy(ctx context.Context, ws *Workspace, vars map[string]string, onProgress func(ProgressEvent)) error
```

---

## Embedding a provider in-process (no network access)

Instead of downloading a provider binary at runtime, you can import the provider's Go module
and run it in-process. OpenTofu connects to it via gRPC reattach — no binary download, no
registry contact for that provider.

### 1. Start the provider server

The exact call depends on which plugin SDK the provider uses.

**terraform-plugin-sdk/v2** (e.g. ionoscloud):

```go
import (
    "github.com/hashicorp/go-plugin"
    tfplugin "github.com/hashicorp/terraform-plugin-sdk/v2/plugin"
    ionoscloud "github.com/ionos-cloud/terraform-provider-ionoscloud/v6/provider"
)

reattachCh := make(chan *plugin.ReattachConfig, 1)
closeCh     := make(chan struct{})

go tfplugin.Serve(&tfplugin.ServeOpts{
    ProviderFunc: ionoscloud.Provider,
    TestConfig: &tfplugin.TestConfig{
        ReattachConfigCh: reattachCh,
        CloseCh:          closeCh,
    },
})

reattach := <-reattachCh
defer close(closeCh)
```

**terraform-plugin-framework**:

```go
import (
    "github.com/hashicorp/go-plugin"
    "github.com/hashicorp/terraform-plugin-go/tfprotov6/tf6server"
    "github.com/example/terraform-provider-foo/internal/provider"
)

reattachCh := make(chan *plugin.ReattachConfig, 1)
closeCh     := make(chan struct{})

go tf6server.Serve("registry.terraform.io/example/foo", provider.New,
    tf6server.WithManagedDebug(reattachCh, closeCh),
)

reattach := <-reattachCh
defer close(closeCh)
```

### 2. Pass the reattach config to the runner

```go
runner, err := embed.New(embed.RunnerConfig{
    UnmanagedProviders: []embed.UnmanagedProvider{
        {
            Source:   "ionos-cloud/ionoscloud",
            Reattach: reattach,
        },
    },
})
if err != nil { ... }
```

TF config must declare the provider with `version` constraints (init validates them):

```hcl
terraform {
  required_providers {
    ionoscloud = {
      source  = "ionos-cloud/ionoscloud"
      version = "~> 2.0"
    }
  }
}
```

---

## Usage example

```go
runner, err := embed.New(embed.RunnerConfig{})
if err != nil { ... }

ws, err := embed.NewWorkspaceFromString(`
resource "ionoscloud_server" "web" { ... }
`)
if err != nil { ... }
defer ws.Close()

ws.StateStore = myDB // implement StateStore to persist state

ctx := context.Background()
if err := runner.Init(ctx, ws); err != nil { ... }

plan, err := runner.Plan(ctx, ws, map[string]string{"datacenter_id": id})
if err != nil { ... }
if plan.HasChanges {
    err = runner.Apply(ctx, ws, map[string]string{"datacenter_id": id}, func(e embed.ProgressEvent) {
        log.Printf("%s %s done=%v", e.Action, e.Addr, e.Done)
    })
}
```

---

## Using as a Go module dependency

The fork keeps the original module path (`github.com/opentofu/opentofu`) so all internal package
imports resolve without modification. Downstream clients reference the fork via a `replace`
directive:

```go
// go.mod
require github.com/opentofu/opentofu v<tag>

replace github.com/opentofu/opentofu => github.com/ionos-landgraf-vin/opentofu v<tag>
```

After adding the directive, run:

```sh
go mod tidy
```

This pulls in all transitive dependencies, including any custom `replace` directives declared in
the fork's own `go.mod` (e.g. a patched HCL library).

---

## Testing

The integration test in `pkg/embed/runner_test.go` exercises the full lifecycle using the
built-in `terraform_data` resource (no provider binary or network access required):

```sh
go test ./pkg/embed/... -v -run TestInMemoryStateStore
```

The test uses an in-memory `StateStore` and asserts that:
- `LoadState`/`SaveState` are called at exactly the right points
- A second Plan after Apply returns `HasChanges=false`, proving the state round-trip works

Expected runtime: a few seconds. No network access.

> **Note:** OpenTofu resolves configuration from the process working directory. The test uses
> `t.Chdir` to point the process at the temp workspace before running commands. Callers in
> production code should ensure their process CWD matches the workspace directory, or use
> `NewWorkspaceFromDir` with a pre-existing directory that is the process CWD.

---

## Concurrency

`EmbeddedRunner` is safe to create once and reuse across goroutines. The backend registry
(`backendInit.Init`) is initialized exactly once via `sync.Once` inside `New`.

Provider plugins still run as **child processes** via `go-plugin` and inherit environment
variables from the parent process. If concurrent operations require isolated credentials via
environment variables (e.g. per-tenant API tokens), the caller is responsible for serializing
those operations — for example, with a `sync.Mutex` held for the duration of each operation
sequence.

---

## Staying in sync with upstream

Periodically rebase this fork onto upstream OpenTofu releases. Because the fork-specific files
are limited to `pkg/embed/` and `docs/ionos.md`, rebases should be low-friction. All other
changes should be upstreamed or avoided.

---

## Known limitations

- Only the **local backend** is used; state is stored in `terraform.tfstate` in the workspace
  directory.
- Remote backends and state locking are not tested.
- Interactive input is disabled (`-input=false`); configurations requiring user prompts will
  fail.
- The process working directory must match the workspace directory when running commands (see
  Testing note above).
