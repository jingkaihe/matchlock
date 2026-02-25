package policy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
)

const (
	networkHookPhaseBefore = "before"
	networkHookPhaseAfter  = "after"

	networkHookActionAllow  = "allow"
	networkHookActionBlock  = "block"
	networkHookActionMutate = "mutate"
)

type compiledNetworkRule struct {
	phase  string
	action string

	callbackID      string
	callbackTimeout time.Duration

	hosts   []string
	methods map[string]struct{}
	path    string

	setHeaders    map[string]string
	deleteHeaders []string
	setQuery      map[string]string
	deleteQuery   []string
	rewritePath   string

	setResponseHeaders    map[string]string
	deleteResponseHeaders []string
	bodyReplacements      []api.NetworkBodyTransform
}

func compileNetworkRules(cfg *api.NetworkInterceptionConfig) []compiledNetworkRule {
	if cfg == nil || len(cfg.Rules) == 0 {
		return nil
	}

	compiled := make([]compiledNetworkRule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		phase := normalizeNetworkHookPhase(rule.Phase)
		action := normalizeNetworkHookAction(rule.Action)
		hasMutations := hasNetworkMutations(rule, phase)
		if action == networkHookActionAllow && hasMutations {
			action = networkHookActionMutate
		}

		cr := compiledNetworkRule{
			phase:  phase,
			action: action,

			callbackID: strings.TrimSpace(rule.CallbackID),

			path:        strings.TrimSpace(rule.Path),
			rewritePath: strings.TrimSpace(rule.RewritePath),

			setHeaders:            normalizeHeaderSet(rule.SetHeaders),
			deleteHeaders:         normalizeHeaderDelete(rule.DeleteHeaders),
			setQuery:              normalizeStringMap(rule.SetQuery),
			deleteQuery:           normalizeStringList(rule.DeleteQuery),
			setResponseHeaders:    normalizeHeaderSet(rule.SetResponseHeaders),
			deleteResponseHeaders: normalizeHeaderDelete(rule.DeleteResponseHeaders),
			bodyReplacements:      normalizeBodyReplacements(rule.BodyReplacements),
		}
		if rule.TimeoutMS > 0 {
			cr.callbackTimeout = time.Duration(rule.TimeoutMS) * time.Millisecond
		}

		for _, host := range rule.Hosts {
			host = strings.TrimSpace(host)
			if host != "" {
				cr.hosts = append(cr.hosts, host)
			}
		}
		if len(rule.Methods) > 0 {
			cr.methods = make(map[string]struct{}, len(rule.Methods))
			for _, method := range rule.Methods {
				method = strings.TrimSpace(strings.ToUpper(method))
				if method != "" {
					cr.methods[method] = struct{}{}
				}
			}
		}

		// Keep only effective rules.
		if cr.action != networkHookActionBlock && !compiledRuleHasMutations(cr, phase) && cr.callbackID == "" {
			continue
		}
		compiled = append(compiled, cr)
	}

	return compiled
}

func normalizeNetworkHookPhase(phase string) string {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case networkHookPhaseAfter:
		return networkHookPhaseAfter
	default:
		return networkHookPhaseBefore
	}
}

func normalizeNetworkHookAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case networkHookActionBlock:
		return networkHookActionBlock
	case networkHookActionMutate:
		return networkHookActionMutate
	default:
		return networkHookActionAllow
	}
}

func hasNetworkMutations(rule api.NetworkHookRule, phase string) bool {
	if phase == networkHookPhaseBefore {
		return len(rule.SetHeaders) > 0 ||
			len(rule.DeleteHeaders) > 0 ||
			len(rule.SetQuery) > 0 ||
			len(rule.DeleteQuery) > 0 ||
			strings.TrimSpace(rule.RewritePath) != ""
	}
	return len(rule.SetResponseHeaders) > 0 ||
		len(rule.DeleteResponseHeaders) > 0 ||
		len(rule.BodyReplacements) > 0
}

func compiledRuleHasMutations(rule compiledNetworkRule, phase string) bool {
	if phase == networkHookPhaseBefore {
		return len(rule.setHeaders) > 0 ||
			len(rule.deleteHeaders) > 0 ||
			len(rule.setQuery) > 0 ||
			len(rule.deleteQuery) > 0 ||
			rule.rewritePath != ""
	}
	return len(rule.setResponseHeaders) > 0 ||
		len(rule.deleteResponseHeaders) > 0 ||
		len(rule.bodyReplacements) > 0
}

func normalizeHeaderSet(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		out[http.CanonicalHeaderKey(key)] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeHeaderDelete(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, k := range in {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		key = http.CanonicalHeaderKey(key)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		out[key] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeStringList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, x := range in {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeBodyReplacements(in []api.NetworkBodyTransform) []api.NetworkBodyTransform {
	if len(in) == 0 {
		return nil
	}
	out := make([]api.NetworkBodyTransform, 0, len(in))
	for _, item := range in {
		if item.Find == "" {
			continue
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeHeaderValues(in map[string][]string) map[string][]string {
	if in == nil {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, values := range in {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		out[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	if len(out) == 0 {
		return map[string][]string{}
	}
	return out
}

func normalizeStringMapPreserveEmpty(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		out[key] = v
	}
	if len(out) == 0 {
		return map[string]string{}
	}
	return out
}

func normalizeCallbackRequestMutation(in *api.NetworkHookRequestMutation) *api.NetworkHookRequestMutation {
	if in == nil {
		return nil
	}
	out := &api.NetworkHookRequestMutation{
		Headers: normalizeHeaderValues(in.Headers),
		Query:   normalizeStringMapPreserveEmpty(in.Query),
		Path:    strings.TrimSpace(in.Path),
	}
	if out.Headers == nil && out.Query == nil && out.Path == "" {
		return nil
	}
	return out
}

func normalizeCallbackResponseMutation(in *api.NetworkHookResponseMutation) *api.NetworkHookResponseMutation {
	if in == nil {
		return nil
	}
	out := &api.NetworkHookResponseMutation{
		Headers:          normalizeHeaderValues(in.Headers),
		BodyReplacements: normalizeBodyReplacements(in.BodyReplacements),
	}
	if in.SetBodyBase64 != nil {
		setBodyBase64 := *in.SetBodyBase64
		out.SetBodyBase64 = &setBodyBase64
	}
	if out.Headers == nil && len(out.BodyReplacements) == 0 && out.SetBodyBase64 == nil {
		return nil
	}
	return out
}

func (r compiledNetworkRule) matches(req *http.Request, host string) bool {
	if len(r.hosts) > 0 {
		matched := false
		for _, pattern := range r.hosts {
			if matchGlob(pattern, host) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	if len(r.methods) > 0 && req != nil {
		method := strings.TrimSpace(strings.ToUpper(req.Method))
		if _, ok := r.methods[method]; !ok {
			return false
		}
	}

	if r.path != "" && req != nil && req.URL != nil {
		pathname := req.URL.Path
		if pathname == "" {
			pathname = "/"
		}
		ok, err := path.Match(r.path, pathname)
		if err != nil || !ok {
			return false
		}
	}

	return true
}

func (e *Engine) applyBeforeNetworkRules(req *http.Request, host string) error {
	if req == nil {
		return nil
	}

	for _, rule := range e.networkRules {
		if rule.phase != networkHookPhaseBefore || !rule.matches(req, host) {
			continue
		}
		if rule.action == networkHookActionBlock {
			return api.ErrBlocked
		}
		applyBeforeMutations(req, rule.setHeaders, rule.deleteHeaders, rule.setQuery, rule.deleteQuery, rule.rewritePath)

		if rule.callbackID == "" || e.networkHook == nil {
			continue
		}

		cbReq := &api.NetworkHookCallbackRequest{
			CallbackID:     rule.callbackID,
			Phase:          networkHookPhaseBefore,
			Host:           host,
			Method:         req.Method,
			Path:           requestPath(req),
			Query:          requestQuery(req),
			RequestHeaders: cloneHeaders(req.Header),
		}
		ctx := context.Background()
		cancel := func() {}
		if rule.callbackTimeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, rule.callbackTimeout)
		}
		cbResp, err := e.networkHook.Invoke(ctx, cbReq)
		cancel()
		if err != nil || cbResp == nil {
			continue
		}
		mutation := normalizeCallbackRequestMutation(cbResp.Request)
		hasMutations := mutation != nil
		action := callbackActionWithMutations(cbResp.Action, hasMutations)
		if action == networkHookActionBlock {
			return api.ErrBlocked
		} else if action == networkHookActionMutate || hasMutations {
			applyCallbackRequestMutation(req, mutation)
		}
	}

	return nil
}

func (e *Engine) applyAfterNetworkRules(resp *http.Response, req *http.Request, host string) (*http.Response, error) {
	if resp == nil {
		return resp, nil
	}

	var replacements []api.NetworkBodyTransform
	var setBody []byte
	hasSetBody := false
	for _, rule := range e.networkRules {
		if rule.phase != networkHookPhaseAfter || !rule.matches(req, host) {
			continue
		}
		if rule.action == networkHookActionBlock {
			return nil, api.ErrBlocked
		}
		for k, v := range rule.setResponseHeaders {
			resp.Header.Set(k, v)
		}
		for _, k := range rule.deleteResponseHeaders {
			resp.Header.Del(k)
		}
		if len(rule.bodyReplacements) > 0 {
			replacements = append(replacements, rule.bodyReplacements...)
		}

		if rule.callbackID == "" || e.networkHook == nil {
			continue
		}

		cbReq := &api.NetworkHookCallbackRequest{
			CallbackID:      rule.callbackID,
			Phase:           networkHookPhaseAfter,
			Host:            host,
			Method:          requestMethod(req),
			Path:            requestPath(req),
			Query:           requestQuery(req),
			RequestHeaders:  requestHeaders(req),
			StatusCode:      resp.StatusCode,
			ResponseHeaders: cloneHeaders(resp.Header),
			IsSSE:           isSSEContentType(resp.Header.Get("Content-Type")),
		}
		ctx := context.Background()
		cancel := func() {}
		if rule.callbackTimeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, rule.callbackTimeout)
		}
		cbResp, err := e.networkHook.Invoke(ctx, cbReq)
		cancel()
		if err != nil || cbResp == nil {
			continue
		}
		mutation := normalizeCallbackResponseMutation(cbResp.Response)
		hasMutations := mutation != nil
		action := callbackActionWithMutations(cbResp.Action, hasMutations)
		if action == networkHookActionBlock {
			return nil, api.ErrBlocked
		}
		if action != networkHookActionMutate && !hasMutations {
			continue
		}
		if mutation != nil {
			if mutation.Headers != nil {
				resp.Header = toHTTPHeader(mutation.Headers)
			}
			if cbBody, ok := decodeBase64Body(mutation.SetBodyBase64); ok {
				setBody = cbBody
				hasSetBody = true
			}
			replacements = append(replacements, mutation.BodyReplacements...)
		}
	}

	if hasSetBody {
		setBody = applyBodyReplacements(setBody, replacements)
		resp.Body = io.NopCloser(bytes.NewReader(setBody))
		resp.ContentLength = int64(len(setBody))
		resp.Header.Set("Content-Length", strconv.Itoa(len(setBody)))
		resp.Header.Del("Transfer-Encoding")
		resp.TransferEncoding = nil
		return resp, nil
	}

	if len(replacements) == 0 || resp.Body == nil {
		return resp, nil
	}

	if isSSEContentType(resp.Header.Get("Content-Type")) {
		resp.Body = newSSETransformReadCloser(resp.Body, replacements)
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		resp.Header.Del("Transfer-Encoding")
		resp.TransferEncoding = nil
		return resp, nil
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	body = applyBodyReplacements(body, replacements)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("Transfer-Encoding")
	resp.TransferEncoding = nil
	return resp, nil
}

func callbackActionWithMutations(action string, hasMutations bool) string {
	normalized := normalizeNetworkHookAction(action)
	if normalized == networkHookActionAllow && hasMutations {
		return networkHookActionMutate
	}
	return normalized
}

func applyBeforeMutations(req *http.Request, setHeaders map[string]string, deleteHeaders []string, setQuery map[string]string, deleteQuery []string, rewritePath string) {
	if req == nil {
		return
	}
	for k, v := range setHeaders {
		req.Header.Set(k, v)
	}
	for _, k := range deleteHeaders {
		req.Header.Del(k)
	}

	if req.URL != nil {
		if len(setQuery) > 0 || len(deleteQuery) > 0 {
			q := req.URL.Query()
			for key, val := range setQuery {
				q.Set(key, val)
			}
			for _, key := range deleteQuery {
				q.Del(key)
			}
			req.URL.RawQuery = q.Encode()
		}
		if rewritePath != "" {
			req.URL.Path = rewritePath
			req.URL.RawPath = ""
		}
	}
}

func applyCallbackRequestMutation(req *http.Request, mutation *api.NetworkHookRequestMutation) {
	if req == nil || mutation == nil {
		return
	}

	if mutation.Headers != nil {
		req.Header = toHTTPHeader(mutation.Headers)
	}

	if req.URL != nil {
		if mutation.Query != nil {
			q := req.URL.Query()
			for key := range q {
				q.Del(key)
			}
			for key, val := range mutation.Query {
				q.Set(key, val)
			}
			req.URL.RawQuery = q.Encode()
		}
		if mutation.Path != "" {
			req.URL.Path = mutation.Path
			req.URL.RawPath = ""
		}
	}
}

func requestMethod(req *http.Request) string {
	if req == nil {
		return ""
	}
	return req.Method
}

func requestPath(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return req.URL.Path
}

func requestQuery(req *http.Request) map[string]string {
	if req == nil || req.URL == nil {
		return nil
	}
	values := req.URL.Query()
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		if len(v) == 0 {
			out[k] = ""
			continue
		}
		out[k] = v[0]
	}
	return out
}

func requestHeaders(req *http.Request) map[string][]string {
	if req == nil {
		return nil
	}
	return cloneHeaders(req.Header)
}

func cloneHeaders(in http.Header) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, values := range in {
		out[k] = append([]string(nil), values...)
	}
	return out
}

func toHTTPHeader(in map[string][]string) http.Header {
	if in == nil {
		return nil
	}
	out := make(http.Header, len(in))
	for k, values := range in {
		out[k] = append([]string(nil), values...)
	}
	return out
}

func decodeBase64Body(data *string) ([]byte, bool) {
	if data == nil {
		return nil, false
	}
	body, err := base64.StdEncoding.DecodeString(*data)
	if err != nil {
		return nil, false
	}
	return body, true
}

func isSSEContentType(contentType string) bool {
	contentType = strings.TrimSpace(strings.ToLower(contentType))
	return strings.HasPrefix(contentType, "text/event-stream")
}

func applyBodyReplacements(body []byte, replacements []api.NetworkBodyTransform) []byte {
	out := body
	for _, item := range replacements {
		if item.Find == "" {
			continue
		}
		out = bytes.ReplaceAll(out, []byte(item.Find), []byte(item.Replace))
	}
	return out
}

type sseTransformReadCloser struct {
	reader       *bufio.Reader
	closer       io.Closer
	replacements []api.NetworkBodyTransform
	pending      []byte
	eof          bool
}

func newSSETransformReadCloser(body io.ReadCloser, replacements []api.NetworkBodyTransform) io.ReadCloser {
	return &sseTransformReadCloser{
		reader:       bufio.NewReader(body),
		closer:       body,
		replacements: replacements,
	}
}

func (r *sseTransformReadCloser) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	for len(r.pending) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		line, err := r.reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				r.eof = true
				if len(line) == 0 {
					return 0, io.EOF
				}
			} else {
				return 0, err
			}
		}
		r.pending = transformSSEDataLine(line, r.replacements)
	}

	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *sseTransformReadCloser) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

func transformSSEDataLine(line []byte, replacements []api.NetworkBodyTransform) []byte {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return line
	}

	payload := line[len("data:"):]
	suffix := []byte(nil)
	switch {
	case bytes.HasSuffix(payload, []byte("\r\n")):
		suffix = []byte("\r\n")
		payload = payload[:len(payload)-2]
	case bytes.HasSuffix(payload, []byte("\n")):
		suffix = []byte("\n")
		payload = payload[:len(payload)-1]
	}
	payload = applyBodyReplacements(payload, replacements)

	out := make([]byte, 0, len("data:")+len(payload)+len(suffix))
	out = append(out, []byte("data:")...)
	out = append(out, payload...)
	out = append(out, suffix...)
	return out
}
