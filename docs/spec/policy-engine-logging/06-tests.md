# 06 - Test Specifications

## Testing Strategy

Three layers of testing:

1. **Unit tests** -- pure logic, no file I/O, no OS dependencies
2. **Integration tests** -- file I/O, concurrent writes, end-to-end serialization
3. **Golden file tests** -- format stability for JSON-L output

All tests use `testify/require` for hard preconditions and `testify/assert` for follow-on checks, per `AGENTS.md:73-74`.

## Test File Inventory

| Test File | Tests | Layer |
|---|---|---|
| `pkg/logging/event_test.go` | Event serialization, field names, omitempty | Unit |
| `pkg/logging/jsonl_writer_test.go` | File creation, append, concurrent writes, close | Integration |
| `pkg/logging/emitter_test.go` | Metadata stamping, sink dispatch, close, nil safety | Unit |
| `pkg/logging/golden_test.go` | Golden file comparison for JSON-L format stability | Golden |
| `pkg/logging/testdata/event_full.golden` | Expected JSON for a fully-populated event | Data |
| `pkg/logging/testdata/event_minimal.golden` | Expected JSON for a minimal event | Data |
| `pkg/policy/engine_test.go` | Existing tests updated to pass nil emitter | Regression |
| `pkg/policy/secret_injector_test.go` | Existing + new emission tests | Unit |
| `pkg/policy/registry_test.go` | Factory signature compatibility | Regression |

## Test Pattern References

- **Test Structure:** Follow `pkg/policy/engine_test.go` -- table-driven tests with `t.Run` subtests
- **Assertions:** `testify/require` + `testify/assert` per `AGENTS.md`
- **Test Fixtures:** No external fixtures needed; all test data is inline
- **Temp Dirs:** Use `t.TempDir()` for file I/O tests (auto-cleaned)

## Unit Tests: pkg/logging/event_test.go

```go
package logging

import (
    "encoding/json"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestEvent_JSONFieldNames(t *testing.T) {
    event := &Event{
        Timestamp:   time.Date(2026, 2, 23, 14, 30, 0, 123000000, time.UTC),
        RunID:       "session-9f8e7d6c",
        AgentSystem: "openclaw",
        EventType:   EventLLMRequest,
        Summary:     "POST api.anthropic.com/v1/messages",
    }
    b, err := json.Marshal(event)
    require.NoError(t, err)

    var m map[string]interface{}
    require.NoError(t, json.Unmarshal(b, &m))

    assert.Contains(t, m, "ts")
    assert.Contains(t, m, "run_id")
    assert.Contains(t, m, "agent_system")
    assert.Contains(t, m, "event_type")
    assert.Contains(t, m, "summary")
    // Omitempty fields absent
    assert.NotContains(t, m, "plugin")
    assert.NotContains(t, m, "tags")
    assert.NotContains(t, m, "data")
}

func TestEvent_OmitemptyPresent(t *testing.T) {
    event := &Event{
        Timestamp:   time.Now().UTC(),
        RunID:       "test",
        AgentSystem: "test",
        EventType:   EventKeyInjection,
        Summary:     "test",
        Plugin:      "secret_injector",
        Tags:        []string{"tls"},
        Data:        json.RawMessage(`{"action":"injected"}`),
    }
    b, err := json.Marshal(event)
    require.NoError(t, err)

    var m map[string]interface{}
    require.NoError(t, json.Unmarshal(b, &m))

    assert.Contains(t, m, "plugin")
    assert.Contains(t, m, "tags")
    assert.Contains(t, m, "data")
}

func TestEvent_TimestampFormat(t *testing.T) {
    ts := time.Date(2026, 2, 23, 14, 30, 0, 123456789, time.UTC)
    event := &Event{Timestamp: ts, RunID: "r", AgentSystem: "a", EventType: "t", Summary: "s"}

    b, err := json.Marshal(event)
    require.NoError(t, err)

    // Verify RFC 3339 with sub-second precision
    var m map[string]interface{}
    require.NoError(t, json.Unmarshal(b, &m))
    tsStr := m["ts"].(string)
    parsed, err := time.Parse(time.RFC3339Nano, tsStr)
    require.NoError(t, err)
    assert.True(t, parsed.Equal(ts))
}

func TestLLMRequestData_RoutedNotOmitted(t *testing.T) {
    data := &LLMRequestData{
        Method: "POST",
        Host:   "api.anthropic.com",
        Path:   "/v1/messages",
        Routed: false,
    }
    b, err := json.Marshal(data)
    require.NoError(t, err)

    var m map[string]interface{}
    require.NoError(t, json.Unmarshal(b, &m))
    assert.Contains(t, m, "routed", "routed field must be present even when false")
    assert.Equal(t, false, m["routed"])
}

func TestKeyInjectionData_ActionAlwaysPresent(t *testing.T) {
    data := &KeyInjectionData{
        SecretName: "API_KEY",
        Host:       "api.example.com",
        Action:     "injected",
    }
    b, err := json.Marshal(data)
    require.NoError(t, err)

    var m map[string]interface{}
    require.NoError(t, json.Unmarshal(b, &m))
    assert.Contains(t, m, "action")
}

func TestEventTypeConstants(t *testing.T) {
    assert.Equal(t, "llm_request", EventLLMRequest)
    assert.Equal(t, "llm_response", EventLLMResponse)
    assert.Equal(t, "budget_action", EventBudgetAction)
    assert.Equal(t, "key_injection", EventKeyInjection)
}
```

## Integration Tests: pkg/logging/jsonl_writer_test.go

```go
package logging

import (
    "bufio"
    "encoding/json"
    "os"
    "path/filepath"
    "sync"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestJSONLWriter_CreatesFile(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "events.jsonl")

    w, err := NewJSONLWriter(path)
    require.NoError(t, err)
    defer w.Close()

    _, err = os.Stat(path)
    assert.NoError(t, err, "file should exist")
}

func TestJSONLWriter_AppendsToExisting(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "events.jsonl")

    // Write first event
    w1, err := NewJSONLWriter(path)
    require.NoError(t, err)
    require.NoError(t, w1.Write(testEvent("first")))
    require.NoError(t, w1.Close())

    // Write second event (new writer, same file)
    w2, err := NewJSONLWriter(path)
    require.NoError(t, err)
    require.NoError(t, w2.Write(testEvent("second")))
    require.NoError(t, w2.Close())

    // Verify both lines exist
    lines := readLines(t, path)
    assert.Len(t, lines, 2)
}

func TestJSONLWriter_ValidJSON(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "events.jsonl")

    w, err := NewJSONLWriter(path)
    require.NoError(t, err)

    require.NoError(t, w.Write(testEvent("test")))
    require.NoError(t, w.Close())

    lines := readLines(t, path)
    require.Len(t, lines, 1)

    var event Event
    require.NoError(t, json.Unmarshal([]byte(lines[0]), &event))
    assert.Equal(t, "test", event.Summary)
}

func TestJSONLWriter_ConcurrentWrites(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "events.jsonl")

    w, err := NewJSONLWriter(path)
    require.NoError(t, err)

    const goroutines = 100
    const eventsPerGoroutine = 10

    var wg sync.WaitGroup
    for i := 0; i < goroutines; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            for j := 0; j < eventsPerGoroutine; j++ {
                _ = w.Write(testEvent("concurrent"))
            }
        }(i)
    }
    wg.Wait()
    require.NoError(t, w.Close())

    lines := readLines(t, path)
    assert.Len(t, lines, goroutines*eventsPerGoroutine)

    // Verify every line is valid JSON
    for i, line := range lines {
        var event Event
        assert.NoError(t, json.Unmarshal([]byte(line), &event),
            "line %d should be valid JSON", i)
    }
}

func TestJSONLWriter_MissingParentDir(t *testing.T) {
    path := filepath.Join(t.TempDir(), "nonexistent", "subdir", "events.jsonl")
    _, err := NewJSONLWriter(path)
    assert.Error(t, err, "should fail when parent directory does not exist")
}

// -- helpers --

func testEvent(summary string) *Event {
    return &Event{
        Timestamp:   time.Now().UTC(),
        RunID:       "test-run",
        AgentSystem: "test",
        EventType:   EventLLMRequest,
        Summary:     summary,
    }
}

func readLines(t *testing.T, path string) []string {
    t.Helper()
    f, err := os.Open(path)
    require.NoError(t, err)
    defer f.Close()

    var lines []string
    scanner := bufio.NewScanner(f)
    for scanner.Scan() {
        if line := scanner.Text(); line != "" {
            lines = append(lines, line)
        }
    }
    require.NoError(t, scanner.Err())
    return lines
}
```

## Unit Tests: pkg/logging/emitter_test.go

```go
package logging

import (
    "encoding/json"
    "errors"
    "sync"
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// captureSink records events in memory for test assertions.
type captureSink struct {
    mu     sync.Mutex
    events []*Event
    closed bool
}

func (s *captureSink) Write(event *Event) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    // Deep copy the event to avoid test races
    cp := *event
    s.events = append(s.events, &cp)
    return nil
}

func (s *captureSink) Close() error {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.closed = true
    return nil
}

func TestEmitter_MetadataStamping(t *testing.T) {
    sink := &captureSink{}
    emitter := NewEmitter(EmitterConfig{
        RunID:       "run-123",
        AgentSystem: "openclaw",
    }, sink)

    err := emitter.Emit(EventLLMRequest, "test summary", "", nil, nil)
    require.NoError(t, err)

    require.Len(t, sink.events, 1)
    event := sink.events[0]
    assert.Equal(t, "run-123", event.RunID)
    assert.Equal(t, "openclaw", event.AgentSystem)
    assert.Equal(t, EventLLMRequest, event.EventType)
    assert.Equal(t, "test summary", event.Summary)
    assert.True(t, event.Timestamp.UTC().Equal(event.Timestamp), "timestamp should be UTC")
}

func TestEmitter_DataMarshaling(t *testing.T) {
    sink := &captureSink{}
    emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"}, sink)

    data := &LLMRequestData{
        Method: "POST",
        Host:   "api.anthropic.com",
        Path:   "/v1/messages",
        Routed: true,
        RoutedTo: "127.0.0.1:11434",
    }
    err := emitter.Emit(EventLLMRequest, "test", "", nil, data)
    require.NoError(t, err)

    require.Len(t, sink.events, 1)
    assert.NotNil(t, sink.events[0].Data)

    var parsed LLMRequestData
    require.NoError(t, json.Unmarshal(sink.events[0].Data, &parsed))
    assert.Equal(t, "POST", parsed.Method)
    assert.True(t, parsed.Routed)
}

func TestEmitter_NilData(t *testing.T) {
    sink := &captureSink{}
    emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"}, sink)

    err := emitter.Emit(EventLLMRequest, "test", "", nil, nil)
    require.NoError(t, err)

    require.Len(t, sink.events, 1)
    assert.Nil(t, sink.events[0].Data)
}

func TestEmitter_MultiSink(t *testing.T) {
    sink1 := &captureSink{}
    sink2 := &captureSink{}
    emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"}, sink1, sink2)

    err := emitter.Emit(EventLLMRequest, "test", "", nil, nil)
    require.NoError(t, err)

    assert.Len(t, sink1.events, 1)
    assert.Len(t, sink2.events, 1)
}

func TestEmitter_NoSinks(t *testing.T) {
    emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"})
    err := emitter.Emit(EventLLMRequest, "test", "", nil, nil)
    assert.NoError(t, err, "emitter with no sinks should not error")
}

type errorSink struct{ err error }

func (s *errorSink) Write(*Event) error { return s.err }
func (s *errorSink) Close() error       { return s.err }

func TestEmitter_SinkErrorPropagation(t *testing.T) {
    sink := &errorSink{err: errors.New("write failed")}
    emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"}, sink)

    err := emitter.Emit(EventLLMRequest, "test", "", nil, nil)
    assert.Error(t, err)
}

func TestEmitter_Close(t *testing.T) {
    sink1 := &captureSink{}
    sink2 := &captureSink{}
    emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"}, sink1, sink2)

    err := emitter.Close()
    assert.NoError(t, err)
    assert.True(t, sink1.closed)
    assert.True(t, sink2.closed)
}

func TestEmitter_CloseErrorCollection(t *testing.T) {
    sink1 := &errorSink{err: errors.New("close1")}
    sink2 := &errorSink{err: errors.New("close2")}
    emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"}, sink1, sink2)

    err := emitter.Close()
    assert.Error(t, err)
    assert.Equal(t, "close1", err.Error(), "should return first error")
}
```

## Golden File Tests: pkg/logging/golden_test.go

Golden file tests ensure the JSON-L format does not accidentally change between releases.

```go
package logging

import (
    "encoding/json"
    "os"
    "path/filepath"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestEvent_GoldenFull(t *testing.T) {
    event := &Event{
        Timestamp:   time.Date(2026, 2, 23, 14, 30, 0, 123000000, time.UTC),
        RunID:       "session-9f8e7d6c",
        AgentSystem: "openclaw",
        EventType:   EventLLMRequest,
        Summary:     "POST api.anthropic.com/v1/messages",
        Plugin:      "secret_injector",
        Tags:        []string{"tls", "mitm"},
        Data:        json.RawMessage(`{"method":"POST","host":"api.anthropic.com","path":"/v1/messages","model":"claude-sonnet-4-20250514","routed":false}`),
    }

    got, err := json.Marshal(event)
    require.NoError(t, err)

    goldenPath := filepath.Join("testdata", "event_full.golden")
    if os.Getenv("UPDATE_GOLDEN") != "" {
        os.MkdirAll("testdata", 0755)
        os.WriteFile(goldenPath, append(got, '\n'), 0644)
        t.Skip("golden file updated")
    }

    expected, err := os.ReadFile(goldenPath)
    require.NoError(t, err, "golden file missing; run with UPDATE_GOLDEN=1 to create")

    assert.JSONEq(t, string(expected), string(got))
}

func TestEvent_GoldenMinimal(t *testing.T) {
    event := &Event{
        Timestamp:   time.Date(2026, 2, 23, 14, 30, 0, 0, time.UTC),
        RunID:       "vm-a1b2c3d4",
        AgentSystem: "unknown",
        EventType:   EventLLMResponse,
        Summary:     "GET example.com/ -> 200",
    }

    got, err := json.Marshal(event)
    require.NoError(t, err)

    goldenPath := filepath.Join("testdata", "event_minimal.golden")
    if os.Getenv("UPDATE_GOLDEN") != "" {
        os.MkdirAll("testdata", 0755)
        os.WriteFile(goldenPath, append(got, '\n'), 0644)
        t.Skip("golden file updated")
    }

    expected, err := os.ReadFile(goldenPath)
    require.NoError(t, err, "golden file missing; run with UPDATE_GOLDEN=1 to create")

    assert.JSONEq(t, string(expected), string(got))
}
```

## Regression Tests

### Existing test files that need updating

The following test files must be updated to pass `nil` as the new emitter parameter:

**`pkg/policy/engine_test.go`:**
- All calls to `NewEngine(config, nil)` become `NewEngine(config, nil, nil)`

**`pkg/policy/secret_injector_test.go`:**
- All calls to `NewSecretInjectorPlugin(secrets, nil)` become `NewSecretInjectorPlugin(secrets, nil, nil)`
- All calls to `NewSecretInjectorPluginFromConfig(raw, nil)` become `NewSecretInjectorPluginFromConfig(raw, nil, nil)`

**`pkg/policy/registry_test.go`:**
- Factory call signatures must match the new `PluginFactory` type

### New tests in existing files

**`pkg/policy/secret_injector_test.go`** -- add emission tests:
- `TestSecretInjectorPlugin_EmitsInjectedEvent`
- `TestSecretInjectorPlugin_EmitsSkippedEvent`
- `TestSecretInjectorPlugin_EmitsLeakBlockedEvent`
- `TestSecretInjectorPlugin_NilEmitterNoEvents`
- `TestSecretInjectorPlugin_NoSecretValuesInEvents`

## Running Tests

```bash
# Run all logging package tests
go test ./pkg/logging/...

# Run with golden file update (first time or after intentional format change)
UPDATE_GOLDEN=1 go test ./pkg/logging/...

# Run all policy tests (includes regression + new emission tests)
go test ./pkg/policy/...

# Run all tests
mise run test
```

## Test Coverage Targets

| Package | Target |
|---|---|
| `pkg/logging` | >90% (new code, fully testable) |
| `pkg/policy` (modified files) | Maintain existing coverage, add emission coverage |
| `pkg/net` (modified files) | Emission code tested via integration; nil-path via existing tests |
