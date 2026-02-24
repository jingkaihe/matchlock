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
		EventType:   EventHTTPRequest,
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
