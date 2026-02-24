package api

// NetworkInterceptionConfig configures host-side HTTP(S) interception hooks.
type NetworkInterceptionConfig struct {
	Rules []NetworkHookRule `json:"rules,omitempty"`
}

// NetworkHookRule describes one network interception rule.
type NetworkHookRule struct {
	Name  string `json:"name,omitempty"`
	Phase string `json:"phase,omitempty"` // before, after

	Hosts   []string `json:"hosts,omitempty"`   // glob patterns; empty matches all hosts
	Methods []string `json:"methods,omitempty"` // HTTP methods; empty matches all methods
	Path    string   `json:"path,omitempty"`    // URL path glob; empty matches all paths

	Action string `json:"action,omitempty"` // allow, block, mutate

	// Before-phase request mutations.
	SetHeaders    map[string]string `json:"set_headers,omitempty"`
	DeleteHeaders []string          `json:"delete_headers,omitempty"`
	SetQuery      map[string]string `json:"set_query,omitempty"`
	DeleteQuery   []string          `json:"delete_query,omitempty"`
	RewritePath   string            `json:"rewrite_path,omitempty"`

	// After-phase response mutations.
	SetResponseHeaders    map[string]string      `json:"set_response_headers,omitempty"`
	DeleteResponseHeaders []string               `json:"delete_response_headers,omitempty"`
	BodyReplacements      []NetworkBodyTransform `json:"body_replacements,omitempty"`
}

// NetworkBodyTransform applies a literal replacement.
// For text/event-stream, replacements are applied to each SSE data line payload.
type NetworkBodyTransform struct {
	Find    string `json:"find"`
	Replace string `json:"replace,omitempty"`
}
