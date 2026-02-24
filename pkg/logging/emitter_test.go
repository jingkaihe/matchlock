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

	err := emitter.Emit(EventHTTPRequest, "test summary", "", nil, nil)
	require.NoError(t, err)

	require.Len(t, sink.events, 1)
	event := sink.events[0]
	assert.Equal(t, "run-123", event.RunID)
	assert.Equal(t, "openclaw", event.AgentSystem)
	assert.Equal(t, EventHTTPRequest, event.EventType)
	assert.Equal(t, "test summary", event.Summary)
	assert.True(t, event.Timestamp.UTC().Equal(event.Timestamp), "timestamp should be UTC")
}

func TestEmitter_DataMarshaling(t *testing.T) {
	sink := &captureSink{}
	emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"}, sink)

	data := &HTTPRequestData{
		Method:   "POST",
		Host:     "api.anthropic.com",
		Path:     "/v1/messages",
		Routed:   true,
		RoutedTo: "127.0.0.1:11434",
	}
	err := emitter.Emit(EventHTTPRequest, "test", "", nil, data)
	require.NoError(t, err)

	require.Len(t, sink.events, 1)
	assert.NotNil(t, sink.events[0].Data)

	var parsed HTTPRequestData
	require.NoError(t, json.Unmarshal(sink.events[0].Data, &parsed))
	assert.Equal(t, "POST", parsed.Method)
	assert.True(t, parsed.Routed)
}

func TestEmitter_NilData(t *testing.T) {
	sink := &captureSink{}
	emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"}, sink)

	err := emitter.Emit(EventHTTPRequest, "test", "", nil, nil)
	require.NoError(t, err)

	require.Len(t, sink.events, 1)
	assert.Nil(t, sink.events[0].Data)
}

func TestEmitter_MultiSink(t *testing.T) {
	sink1 := &captureSink{}
	sink2 := &captureSink{}
	emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"}, sink1, sink2)

	err := emitter.Emit(EventHTTPRequest, "test", "", nil, nil)
	require.NoError(t, err)

	assert.Len(t, sink1.events, 1)
	assert.Len(t, sink2.events, 1)
}

func TestEmitter_NoSinks(t *testing.T) {
	emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"})
	err := emitter.Emit(EventHTTPRequest, "test", "", nil, nil)
	assert.NoError(t, err, "emitter with no sinks should not error")
}

type errorSink struct{ err error }

func (s *errorSink) Write(*Event) error { return s.err }
func (s *errorSink) Close() error       { return s.err }

func TestEmitter_SinkErrorPropagation(t *testing.T) {
	sink := &errorSink{err: errors.New("write failed")}
	emitter := NewEmitter(EmitterConfig{RunID: "r", AgentSystem: "a"}, sink)

	err := emitter.Emit(EventHTTPRequest, "test", "", nil, nil)
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
