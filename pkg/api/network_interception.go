package api

// NetworkInterceptionConfig configures host-side HTTP(S) interception hooks.
type NetworkInterceptionConfig struct {
	Rules []NetworkHookRule `json:"rules,omitempty"`
	// CallbackSocket enables SDK-local callback hooks over a Unix domain socket.
	// This is intended for SDK-internal use.
	CallbackSocket string `json:"callback_socket,omitempty"`
}

// NetworkHookRule describes one network interception rule.
type NetworkHookRule struct {
	Name  string `json:"name,omitempty"`
	Phase string `json:"phase,omitempty"` // before, after

	Hosts   []string `json:"hosts,omitempty"`   // glob patterns; empty matches all hosts
	Methods []string `json:"methods,omitempty"` // HTTP methods; empty matches all methods
	Path    string   `json:"path,omitempty"`    // URL path glob; empty matches all paths

	Action string `json:"action,omitempty"` // allow, block, mutate
	// CallbackID identifies an SDK-local callback hook for this rule.
	// This is intended for SDK-internal use.
	CallbackID string `json:"callback_id,omitempty"`
	// TimeoutMS applies to SDK-local callback execution.
	TimeoutMS int `json:"timeout_ms,omitempty"`

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

// NetworkHookCallbackRequest is sent from host interception logic to an SDK-local
// callback server when a matching callback rule is triggered.
type NetworkHookCallbackRequest struct {
	CallbackID string `json:"callback_id"`
	Phase      string `json:"phase"`
	Host       string `json:"host,omitempty"`
	Method     string `json:"method,omitempty"`
	Path       string `json:"path,omitempty"`

	Query          map[string]string   `json:"query,omitempty"`
	RequestHeaders map[string][]string `json:"request_headers,omitempty"`

	StatusCode      int                 `json:"status_code,omitempty"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	IsSSE           bool                `json:"is_sse,omitempty"`
}

// NetworkHookRequestMutation describes request-shaping changes returned by an
// SDK-local callback.
type NetworkHookRequestMutation struct {
	// Headers replaces the full outbound request header map when non-nil.
	Headers map[string][]string `json:"headers,omitempty"`
	// Query replaces the full outbound query map when non-nil.
	Query map[string]string `json:"query,omitempty"`
	// Path rewrites the outbound request path when non-empty.
	Path string `json:"path,omitempty"`
}

// NetworkHookResponseMutation describes response-shaping changes returned by an
// SDK-local callback.
type NetworkHookResponseMutation struct {
	// Headers replaces the full inbound response header map when non-nil.
	Headers          map[string][]string    `json:"headers,omitempty"`
	BodyReplacements []NetworkBodyTransform `json:"body_replacements,omitempty"`
	// SetBodyBase64 replaces the entire response body when set.
	SetBodyBase64 string `json:"set_body_base64,omitempty"`
}

// NetworkHookCallbackResponse is returned by an SDK-local callback server.
type NetworkHookCallbackResponse struct {
	Action string `json:"action,omitempty"` // allow, block, mutate

	Request  *NetworkHookRequestMutation  `json:"request,omitempty"`
	Response *NetworkHookResponseMutation `json:"response,omitempty"`

	Error string `json:"error,omitempty"`
}
