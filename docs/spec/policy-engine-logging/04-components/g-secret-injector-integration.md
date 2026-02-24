# Component G: Secret Injector Integration

## Purpose

Add `key_injection` event emission to `secretInjectorPlugin.TransformRequest()`. Events are emitted alongside the existing `slog.Debug` calls for each secret interaction: injected, skipped (wrong host), and leak blocked.

## Codebase References

- **Existing Plugin:** `pkg/policy/secret_injector.go:18-140` -- the full plugin implementation
- **Existing slog calls:** Lines 80-87 -- the three `p.logger.Debug(...)` calls that the new events parallel
- **Secret safety:** Line 127 -- `replaceInRequest()` is the only place secret values are handled; they never reach logging

## File Location

`pkg/policy/secret_injector.go`

## Changes Required

### 1. Add emitter field to secretInjectorPlugin

```go
type secretInjectorPlugin struct {
    secrets      map[string]api.Secret
    placeholders map[string]string
    logger       *slog.Logger
    emitter      *logging.Emitter  // NEW: nil means no event logging
}
```

### 2. Update constructors

**Direct constructor:**

```go
func NewSecretInjectorPlugin(secrets map[string]api.Secret, logger *slog.Logger, emitter *logging.Emitter) *secretInjectorPlugin {
    if logger == nil {
        logger = slog.Default()
    }
    p := &secretInjectorPlugin{
        secrets:      make(map[string]api.Secret),
        placeholders: make(map[string]string),
        logger:       logger,
        emitter:      emitter,  // NEW: may be nil
    }
    // ... rest unchanged
}
```

**Factory constructor:**

```go
func NewSecretInjectorPluginFromConfig(raw json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error) {
    if logger == nil {
        logger = slog.Default()
    }
    var cfg SecretInjectorConfig
    if err := json.Unmarshal(raw, &cfg); err != nil {
        return nil, err
    }
    return NewSecretInjectorPlugin(cfg.Secrets, logger, emitter), nil
}
```

### 3. Emit key_injection events in TransformRequest

The three existing `p.logger.Debug(...)` calls each get a parallel emitter call. The `slog` calls remain unchanged -- these are additive.

```go
func (p *secretInjectorPlugin) TransformRequest(req *http.Request, host string) (*http.Request, error) {
    host = strings.Split(host, ":")[0]

    for name, secret := range p.secrets {
        if !p.isSecretAllowedForHost(name, host) {
            if p.requestContainsPlaceholder(req, secret.Placeholder) {
                p.logger.Debug("secret leak detected", "name", name, "host", host)
                // NEW: emit key_injection event for leak
                if p.emitter != nil {
                    _ = p.emitter.Emit(logging.EventKeyInjection,
                        fmt.Sprintf("secret %q leak blocked for %s", name, host),
                        "secret_injector",
                        nil,
                        &logging.KeyInjectionData{
                            SecretName: name,
                            Host:       host,
                            Action:     "leak_blocked",
                        })
                }
                return nil, api.ErrSecretLeak
            }
            p.logger.Debug("secret skipped for host", "name", name, "host", host)
            // NEW: emit key_injection event for skip
            if p.emitter != nil {
                _ = p.emitter.Emit(logging.EventKeyInjection,
                    fmt.Sprintf("secret %q skipped for %s", name, host),
                    "secret_injector",
                    nil,
                    &logging.KeyInjectionData{
                        SecretName: name,
                        Host:       host,
                        Action:     "skipped",
                    })
            }
            continue
        }
        p.replaceInRequest(req, secret.Placeholder, secret.Value)
        p.logger.Debug("secret injected", "name", name, "host", host)
        // NEW: emit key_injection event for injection
        if p.emitter != nil {
            _ = p.emitter.Emit(logging.EventKeyInjection,
                fmt.Sprintf("secret %q injected for %s", name, host),
                "secret_injector",
                nil,
                &logging.KeyInjectionData{
                    SecretName: name,
                    Host:       host,
                    Action:     "injected",
                })
        }
    }

    return req, nil
}
```

### Key Design Points

1. **No secret values in events:** The `KeyInjectionData` struct contains `SecretName` (the env var name like `"ANTHROPIC_API_KEY"`) and `Host`, never the secret value. This is enforced by the struct definition -- there is no `Value` field.

2. **slog calls unchanged:** All existing `p.logger.Debug(...)` calls remain. The emitter calls are additive.

3. **Plugin field on event:** All events set `plugin: "secret_injector"` to identify the source.

4. **Three actions:** `"injected"` (success), `"skipped"` (not allowed for this host), `"leak_blocked"` (placeholder detected going to unauthorized host).

## Dependencies

- `github.com/jingkaihe/matchlock/pkg/logging`
- `fmt` (already imported)
- All existing imports unchanged

## Test Criteria

1. **Nil emitter:** All existing tests pass when emitter is nil (pass `nil` as third arg)
2. **Injection event:** When a secret is injected, a `key_injection` event with `action: "injected"` is emitted
3. **Skip event:** When a secret is skipped (wrong host), a `key_injection` event with `action: "skipped"` is emitted
4. **Leak event:** When a leak is detected, a `key_injection` event with `action: "leak_blocked"` is emitted before the error is returned
5. **No secret values:** Event `SecretName` contains the env var name, never the actual secret value
6. **Plugin field:** All events have `plugin: "secret_injector"`
7. **Summary format:** Summary follows the pattern `secret "NAME" <action> for HOST`

## Test Pattern

**Pattern Reference:** Follow the test structure in `pkg/policy/secret_injector_test.go` which uses direct plugin construction with `NewSecretInjectorPlugin(...)`.

```go
func TestSecretInjectorPlugin_EmitsKeyInjectionEvent(t *testing.T) {
    capture := &captureSink{}
    emitter := logging.NewEmitter(logging.EmitterConfig{
        RunID: "test-run", AgentSystem: "test",
    }, capture)

    p := NewSecretInjectorPlugin(map[string]api.Secret{
        "API_KEY": {
            Value: "real-secret",
            Hosts: []string{"api.example.com"},
        },
    }, nil, emitter)

    placeholder := p.GetPlaceholders()["API_KEY"]
    req := &http.Request{
        Header: http.Header{
            "Authorization": []string{"Bearer " + placeholder},
        },
        URL: &url.URL{},
    }

    _, err := p.TransformRequest(req, "api.example.com")
    require.NoError(t, err)

    require.Len(t, capture.events, 1)
    event := capture.events[0]
    assert.Equal(t, "key_injection", event.EventType)
    assert.Equal(t, "secret_injector", event.Plugin)
    assert.Contains(t, event.Summary, "API_KEY")
    assert.NotContains(t, event.Summary, "real-secret")
    // Verify data
    var data logging.KeyInjectionData
    require.NoError(t, json.Unmarshal(event.Data, &data))
    assert.Equal(t, "API_KEY", data.SecretName)
    assert.Equal(t, "injected", data.Action)
}
```

## Acceptance Criteria

- [ ] `secretInjectorPlugin` has `emitter *logging.Emitter` field
- [ ] Both constructors accept `*logging.Emitter` parameter
- [ ] `TransformRequest` emits `key_injection` for all three actions
- [ ] No secret values appear in any event data or summary
- [ ] Existing `slog.Debug` calls are unchanged (additive only)
- [ ] All existing tests pass with `nil` emitter
- [ ] New tests verify event emission with capture sink
