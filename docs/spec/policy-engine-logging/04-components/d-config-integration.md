# Component D: Config Integration

## Purpose

Add `LoggingConfig` to `api.Config` and wire the emitter construction into the sandbox creation path. This component bridges the configuration layer with the logging layer.

## Codebase References

- **Config Struct Pattern:** `api.NetworkConfig` in `pkg/api/config.go:79-96` -- struct with JSON tags, pointer field on `Config`
- **Sandbox Construction:** `sandbox.New()` in `pkg/sandbox/sandbox_darwin.go:54-389` -- the primary construction site
- **State Manager Base Dir:** `state.NewManager()` in `pkg/state/state.go:30-34` uses `~/.matchlock/vms` as base
- **VM ID Generation:** `config.GetID()` in `pkg/api/config.go:250-254`

## File Locations

- `pkg/api/config.go` -- add `LoggingConfig` struct and field
- `pkg/sandbox/sandbox_darwin.go` -- construct emitter in `New()`
- `pkg/sandbox/sandbox_linux.go` -- construct emitter in `New()`

## Implementation: pkg/api/config.go

Add the `LoggingConfig` struct and a `Logging` field to `Config`:

```go
// LoggingConfig configures the structured event logging system.
type LoggingConfig struct {
    EventLogPath string `json:"event_log_path,omitempty"` // Override file path; empty = default
    Enabled      bool   `json:"enabled,omitempty"`        // Enable JSON-L event logging
    RunID        string `json:"run_id,omitempty"`         // Caller-supplied session ID
    AgentSystem  string `json:"agent_system,omitempty"`   // e.g., "openclaw", "aider"
}
```

Add to `Config` struct (after `ImageCfg` field):

```go
type Config struct {
    ID         string            `json:"id,omitempty"`
    Image      string            `json:"image,omitempty"`
    Privileged bool              `json:"privileged,omitempty"`
    Resources  *Resources        `json:"resources,omitempty"`
    Network    *NetworkConfig    `json:"network,omitempty"`
    VFS        *VFSConfig        `json:"vfs,omitempty"`
    Env        map[string]string `json:"env,omitempty"`
    ExtraDisks []DiskMount       `json:"extra_disks,omitempty"`
    ImageCfg   *ImageConfig      `json:"image_config,omitempty"`
    Logging    *LoggingConfig    `json:"logging,omitempty"`        // NEW
}
```

## Implementation: Sandbox Emitter Construction

In both `sandbox_darwin.go` and `sandbox_linux.go`, add emitter construction after the policy engine is created but before the network stack/proxy is constructed.

### Construction Logic (shared between darwin/linux)

```go
// After: policyEngine := policy.NewEngine(config.Network, nil, nil)
// Before: network stack/proxy construction

var emitter *logging.Emitter
if config.Logging != nil && config.Logging.Enabled {
    // Determine run ID
    runID := config.Logging.RunID
    if runID == "" {
        runID = id // sandbox VM ID
    }

    // Determine log path
    logPath := config.Logging.EventLogPath
    if logPath == "" {
        logDir := filepath.Join(filepath.Dir(stateMgr.Dir(id)), "logs", id)
        if err := os.MkdirAll(logDir, 0755); err != nil {
            // Non-fatal: log the error, proceed without event logging
            slog.Warn("failed to create event log directory", "path", logDir, "error", err)
        } else {
            logPath = filepath.Join(logDir, "events.jsonl")
        }
    }

    if logPath != "" {
        // Ensure parent directory exists for override paths
        if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
            slog.Warn("failed to create event log directory", "path", logPath, "error", err)
        } else {
            writer, err := logging.NewJSONLWriter(logPath)
            if err != nil {
                slog.Warn("failed to create event log writer", "path", logPath, "error", err)
            } else {
                emitter = logging.NewEmitter(logging.EmitterConfig{
                    RunID:       runID,
                    AgentSystem: config.Logging.AgentSystem,
                }, writer)
            }
        }
    }
}
```

### Key Points

1. The default log directory uses `stateMgr.Dir(id)` to derive the base: `filepath.Dir(stateMgr.Dir(id))` gives the base dir (e.g., `~/.matchlock/vms`), then `../logs/<id>/` provides the log path. This keeps logs under the same root as state but in a sibling directory.

2. Actually, to keep it simpler and more predictable: use the state manager's base dir directly. `stateMgr.Dir(id)` returns `~/.matchlock/vms/<id>`. The log path should be `~/.matchlock/logs/<id>/events.jsonl`. Since `stateMgr.baseDir` is `~/.matchlock/vms`, we go one level up to `~/.matchlock/` and then into `logs/`. The implementation should use `filepath.Join(filepath.Dir(stateMgr.baseDir), "logs", id)` to compute this.

3. **Non-fatal construction:** If the log file cannot be created, the sandbox still starts -- the emitter is nil and no events are logged. This matches the best-effort semantics.

### Emitter Wiring

Pass the emitter to `NewEngine` and through the network config:

```go
// Darwin (sandbox_darwin.go)
policyEngine := policy.NewEngine(config.Network, nil, emitter)

netStack, err = sandboxnet.NewNetworkStack(&sandboxnet.Config{
    // ... existing fields ...
    Emitter:    emitter,
})

// Linux (sandbox_linux.go)
policyEngine := policy.NewEngine(config.Network, nil, emitter)

proxy, err = sandboxnet.NewTransparentProxy(&sandboxnet.ProxyConfig{
    // ... existing fields ...
    Emitter:    emitter,
})
```

### Emitter in Sandbox Struct

Add `emitter *logging.Emitter` to the `Sandbox` struct in both darwin and linux files. Close the emitter in `Sandbox.Close()`:

```go
// In Sandbox.Close(), after closing events channel but before state unregister:
if s.emitter != nil {
    if err := s.emitter.Close(); err != nil {
        errs = append(errs, err)
    }
    markCleanup("emitter_close", err)
}
```

### Merge Handling

Add logging config to `Config.Merge()`:

```go
// In Config.Merge()
if other.Logging != nil {
    result.Logging = other.Logging
}
```

## Dependencies

- `pkg/logging` (for `Emitter`, `JSONLWriter`, `EmitterConfig`)
- `pkg/api/config.go` (modified)
- `pkg/state/state.go` (for base directory derivation)
- `log/slog` (for warning on non-fatal failures)
- `os` (for `MkdirAll`)
- `path/filepath` (for path construction)

## Test Criteria

1. **LoggingConfig serialization:** JSON round-trip of `Config` with `Logging` field preserves all fields
2. **LoggingConfig nil:** `Config` with nil `Logging` serializes without the field (omitempty)
3. **Merge:** `Config.Merge()` with non-nil `other.Logging` overwrites result's Logging
4. **Default path derivation:** Given a state dir of `/tmp/test-state/vms`, the default log path is `/tmp/test-state/logs/<id>/events.jsonl`
5. **Override path:** When `EventLogPath` is set, it is used directly
6. **Disabled logging:** When `Enabled` is false, emitter is nil
7. **RunID default:** When `RunID` is empty, it defaults to the sandbox VM ID

## Acceptance Criteria

- [ ] `LoggingConfig` struct added to `pkg/api/config.go`
- [ ] `Logging *LoggingConfig` field added to `Config` struct
- [ ] `Config.Merge()` handles `Logging` field
- [ ] Emitter constructed in both `sandbox_darwin.go` and `sandbox_linux.go`
- [ ] Emitter passed to `policy.NewEngine()` and network config structs
- [ ] Emitter closed in `Sandbox.Close()`
- [ ] Non-fatal: log creation failure does not prevent sandbox from starting
- [ ] `--event-log <path>` implies `Enabled: true`
