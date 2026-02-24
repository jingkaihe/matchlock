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
