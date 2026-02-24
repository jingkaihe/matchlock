# Component H: CLI Flags

## Purpose

Add three CLI flags to `matchlock run` for controlling event logging: `--event-log`, `--run-id`, and `--agent-system`. These flags populate the `LoggingConfig` on `api.Config`.

## Codebase References

- **Existing Flag Registration:** `cmd/matchlock/cmd_run.go:82-141` -- the `init()` function with `runCmd.Flags()` and `viper.BindPFlag()` calls
- **Flag Parsing Pattern:** `cmd/matchlock/cmd_run.go:143-196` -- the `runRun` function's flag extraction section
- **Config Construction:** `cmd/matchlock/cmd_run.go:338-362` -- where `api.Config` is assembled from parsed flags

## File Location

`cmd/matchlock/cmd_run.go`

## Changes Required

### 1. Register flags in init()

Add after the existing `runCmd.Flags()` calls (around line 112):

```go
runCmd.Flags().String("event-log", "", "Path for JSON-L event log (enables event logging)")
runCmd.Flags().String("run-id", "", "Run/session ID for event log (default: sandbox VM ID)")
runCmd.Flags().String("agent-system", "", "Agent system name for event log (e.g., \"openclaw\", \"aider\")")
```

Add viper bindings:

```go
viper.BindPFlag("run.event-log", runCmd.Flags().Lookup("event-log"))
viper.BindPFlag("run.run-id", runCmd.Flags().Lookup("run-id"))
viper.BindPFlag("run.agent-system", runCmd.Flags().Lookup("agent-system"))
```

### 2. Parse flags in runRun

Add after the existing flag parsing section (around line 201):

```go
eventLogPath, _ := cmd.Flags().GetString("event-log")
runID, _ := cmd.Flags().GetString("run-id")
agentSystem, _ := cmd.Flags().GetString("agent-system")
```

### 3. Construct LoggingConfig

Add after the config construction (around line 362), before `sandbox.New()`:

```go
// Event logging config
if eventLogPath != "" || agentSystem != "" {
    config.Logging = &api.LoggingConfig{
        EventLogPath: eventLogPath,
        Enabled:      true,  // --event-log or --agent-system implies enabled
        RunID:        runID,
        AgentSystem:  agentSystem,
    }
}
```

### Design Notes

1. **Implicit enable:** Providing `--event-log <path>` or `--agent-system <name>` implies `Enabled: true`. There is no separate `--enable-event-log` flag because the presence of either flag signals intent to log.

2. **Default behavior:** When no flags are provided, `config.Logging` is nil, and no event logging occurs. Zero behavioral change for existing users.

3. **Run ID default:** The `--run-id` flag is optional. If not provided, the sandbox construction code defaults it to the VM ID (handled in Component D, not here).

4. **Agent system is informational:** Matchlock does not validate the `--agent-system` value. It is stamped as-is onto every event.

5. **Path handling:** If `--event-log` is provided, it is used as the exact file path. If not provided but logging is enabled (e.g., only `--agent-system` is set), the default path `~/.matchlock/logs/<id>/events.jsonl` is used.

### CLI Help Text

The flags should appear in the help output with clear descriptions:

```
Event Logging:
  --event-log string       Path for JSON-L event log (enables event logging)
  --run-id string          Run/session ID for event log (default: sandbox VM ID)
  --agent-system string    Agent system name for event log (e.g., "openclaw", "aider")
```

## Dependencies

- `pkg/api` (for `LoggingConfig` struct)
- No new imports in `cmd_run.go` beyond what already exists

## Test Criteria

1. **No flags:** `config.Logging` is nil when no event-log flags are provided
2. **--event-log only:** `config.Logging.Enabled` is true, `EventLogPath` is set
3. **--agent-system only:** `config.Logging.Enabled` is true, `AgentSystem` is set, `EventLogPath` is empty (uses default)
4. **All three flags:** All fields populated correctly
5. **--run-id without other flags:** Does not enable logging (run-id alone is insufficient)
6. **Help text:** Flags appear in `matchlock run --help` output

Note: Since `runRun` requires an image and complex setup, testing flag parsing may be limited to verifying the config construction logic. Integration testing with actual sandbox creation is covered in the integration test section of `06-tests.md`.

## Acceptance Criteria

- [ ] Three new flags registered in `init()`
- [ ] Viper bindings for all three flags
- [ ] Flags parsed in `runRun`
- [ ] `LoggingConfig` constructed and set on `api.Config`
- [ ] `--event-log` or `--agent-system` implies `Enabled: true`
- [ ] No behavioral change when flags are not provided
- [ ] Help text is clear and accurate
