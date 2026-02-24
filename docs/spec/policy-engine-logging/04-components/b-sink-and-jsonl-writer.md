# Component B: Sink Interface and JSONLWriter

## Purpose

Define the `Sink` interface for event consumers and implement the `JSONLWriter` -- a file-backed sink that writes one JSON object per line to a persistent `.jsonl` file. This is the durable persistence layer.

## Codebase References

- **Concurrency Pattern:** The `sync.Mutex` guarding writes follows the same pattern as `pkg/net/proxy.go:38-39` (`mu sync.Mutex` + `closed bool` on struct)
- **File Operations:** Standard `os.OpenFile` with append mode, consistent with Go idioms used throughout the codebase
- **Error Pattern:** Uses sentinel errors per `AGENTS.md:79-95` and `internal/errx`
- **Close Pattern:** The `Sync()` then `Close()` ordering follows filesystem durability best practices; the existing `sandbox.Close()` in `pkg/sandbox/sandbox_darwin.go:475-582` demonstrates similar cleanup-with-error-collection

## File Locations

- `pkg/logging/sink.go` -- interface definition
- `pkg/logging/jsonl_writer.go` -- implementation
- `pkg/logging/errors.go` -- sentinel errors

## Implementation: sink.go

```go
package logging

// Sink consumes structured events.
// Implementations must be safe for concurrent use.
type Sink interface {
    // Write persists or forwards a single event.
    // Implementations should not modify the event.
    Write(event *Event) error

    // Close flushes any buffered data and releases resources.
    Close() error
}
```

## Implementation: errors.go

**Pattern Reference:** Follows the sentinel error pattern in `pkg/sandbox/errors.go`, `pkg/state/errors.go`, and other package-level error files.

```go
package logging

import "errors"

var (
    ErrCreateLogFile = errors.New("logging: create log file")
    ErrWriteEvent    = errors.New("logging: write event")
    ErrMarshalData   = errors.New("logging: marshal event data")
    ErrCloseWriter   = errors.New("logging: close writer")
)
```

## Implementation: jsonl_writer.go

```go
package logging

import (
    "encoding/json"
    "os"
    "sync"

    "github.com/jingkaihe/matchlock/internal/errx"
)

// JSONLWriter writes structured events as JSON-L to a file.
// It implements Sink and is safe for concurrent use.
type JSONLWriter struct {
    mu   sync.Mutex
    file *os.File
    enc  *json.Encoder
}

// NewJSONLWriter creates a new JSON-L writer that appends to the given file path.
// The parent directory must already exist (caller is responsible for mkdir).
// The file is created if it does not exist.
func NewJSONLWriter(path string) (*JSONLWriter, error) {
    f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
    if err != nil {
        return nil, errx.Wrap(ErrCreateLogFile, err)
    }
    return &JSONLWriter{
        file: f,
        enc:  json.NewEncoder(f),
    }, nil
}

// Write serializes the event as a single JSON line and writes it to the file.
func (w *JSONLWriter) Write(event *Event) error {
    w.mu.Lock()
    defer w.mu.Unlock()
    if err := w.enc.Encode(event); err != nil {
        return errx.Wrap(ErrWriteEvent, err)
    }
    return nil
}

// Close syncs and closes the underlying file.
func (w *JSONLWriter) Close() error {
    w.mu.Lock()
    defer w.mu.Unlock()
    _ = w.file.Sync()
    if err := w.file.Close(); err != nil {
        return errx.Wrap(ErrCloseWriter, err)
    }
    return nil
}
```

## Dependencies

- `encoding/json` (standard library)
- `os` (standard library)
- `sync` (standard library)
- `github.com/jingkaihe/matchlock/internal/errx`
- `pkg/logging/event.go` (Event struct)

## Test Criteria

1. **File creation:** `NewJSONLWriter` creates a file at the given path with `0644` permissions
2. **Append mode:** Writing to an existing file appends, does not truncate
3. **JSON-L format:** Each `Write` call produces exactly one newline-terminated JSON line
4. **Valid JSON:** Every line parses independently as valid JSON
5. **Concurrent writes:** 100 goroutines each writing 10 events produces 1000 valid JSON lines (no interleaving, no corruption)
6. **Close idempotency:** Calling `Close()` twice does not panic (second call returns an error, which is acceptable)
7. **Sync on close:** Verify `Sync()` is called before `Close()` (mock or integration test)
8. **Error wrapping:** Errors are wrapped with the appropriate sentinel (`ErrCreateLogFile`, `ErrWriteEvent`, `ErrCloseWriter`)
9. **Parent directory must exist:** `NewJSONLWriter` fails with a meaningful error if the parent directory does not exist

## Acceptance Criteria

- [ ] `JSONLWriter` implements `Sink` interface
- [ ] File is opened in append mode (`O_CREATE|O_WRONLY|O_APPEND`)
- [ ] `json.Encoder` is used (not `json.Marshal` + manual write) -- this provides newline termination automatically
- [ ] `sync.Mutex` protects all file operations
- [ ] `Sync()` is called in `Close()` before `file.Close()`
- [ ] Sentinel errors follow `internal/errx` pattern
- [ ] No buffering (`bufio.Writer`) -- direct writes to `*os.File`
