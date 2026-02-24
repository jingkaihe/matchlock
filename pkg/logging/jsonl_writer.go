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
