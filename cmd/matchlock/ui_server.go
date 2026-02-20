package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/image"
	"github.com/jingkaihe/matchlock/pkg/lifecycle"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
	"github.com/jingkaihe/matchlock/pkg/state"
)

const (
	defaultUITerminalRows uint16 = 24
	defaultUITerminalCols uint16 = 120
	maxUIRequestBodySize         = 1 << 20

	wsFrameTypeInput  byte = 0
	wsFrameTypeResize byte = 1
)

var errSandboxNotManaged = errors.New("sandbox not managed by this UI server")

type managedSandbox struct {
	sb    *sandbox.Sandbox
	relay *sandbox.ExecRelay
	// cancel controls the VM process context for this managed sandbox.
	cancel context.CancelFunc
}

type uiServer struct {
	assets          fs.FS
	stateMgr        *state.Manager
	lifecycleMgr    *lifecycle.VMManager
	shutdownTimeout time.Duration

	mu      sync.RWMutex
	managed map[string]*managedSandbox
}

type uiSandboxSummary struct {
	ID        string    `json:"id"`
	PID       int       `json:"pid"`
	Status    string    `json:"status"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
	Managed   bool      `json:"managed"`
}

type uiImageSummary struct {
	Tag       string    `json:"tag"`
	Source    string    `json:"source"`
	Digest    string    `json:"digest"`
	SizeMB    float64   `json:"size_mb"`
	CreatedAt time.Time `json:"created_at"`
}

type uiStartSandboxRequest struct {
	Image      string `json:"image"`
	Pull       bool   `json:"pull,omitempty"`
	CPUs       int    `json:"cpus,omitempty"`
	MemoryMB   int    `json:"memory_mb,omitempty"`
	DiskSizeMB int    `json:"disk_size_mb,omitempty"`
	Workspace  string `json:"workspace,omitempty"`
	Privileged bool   `json:"privileged,omitempty"`
}

type uiPullImageRequest struct {
	Image string `json:"image"`
	Force bool   `json:"force,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

type uiDeleteImageRequest struct {
	Tag string `json:"tag"`
}

func newUIServer(shutdownTimeout time.Duration) (*uiServer, error) {
	assets, err := uiAssetsFS()
	if err != nil {
		return nil, errx.Wrap(ErrUIServeAssets, err)
	}

	return &uiServer{
		assets:          assets,
		stateMgr:        state.NewManager(),
		lifecycleMgr:    lifecycle.NewVMManager(),
		shutdownTimeout: shutdownTimeout,
		managed:         make(map[string]*managedSandbox),
	}, nil
}

func (s *uiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sandboxes", s.handleSandboxes)
	mux.HandleFunc("/api/sandboxes/", s.handleSandboxActions)
	mux.HandleFunc("/api/images", s.handleImages)
	mux.HandleFunc("/api/images/pull", s.handlePullImage)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/", s.handleUI)
	return mux
}

func (s *uiServer) Close(ctx context.Context) error {
	s.mu.Lock()
	managed := make(map[string]*managedSandbox, len(s.managed))
	for id, ms := range s.managed {
		managed[id] = ms
	}
	clear(s.managed)
	s.mu.Unlock()

	var errs []error
	for id, ms := range managed {
		if ms.cancel != nil {
			ms.cancel()
		}
		if ms.relay != nil {
			ms.relay.Stop()
		}
		if ms.sb != nil {
			if err := ms.sb.Close(ctx); err != nil {
				errs = append(errs, errx.With(ErrUIStopSandbox, " %s: %v", id, err))
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (s *uiServer) handleSandboxes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListSandboxes(w)
	case http.MethodPost:
		s.handleStartSandbox(w, r)
	default:
		writeMethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *uiServer) handleImages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListImages(w)
	case http.MethodDelete:
		s.handleDeleteImage(w, r)
	default:
		writeMethodNotAllowed(w, http.MethodGet, http.MethodDelete)
	}
}

func (s *uiServer) handleListImages(w http.ResponseWriter) {
	store := image.NewStore("")
	localImages, err := store.List()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUIListImages, err).Error())
		return
	}

	registryImages, err := image.ListRegistryCache("")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUIListImages, err).Error())
		return
	}

	items := make([]uiImageSummary, 0, len(localImages)+len(registryImages))
	for _, img := range localImages {
		source := img.Meta.Source
		if source == "" {
			source = "local"
		}
		items = append(items, uiImageSummary{
			Tag:       img.Tag,
			Source:    source,
			Digest:    img.Meta.Digest,
			SizeMB:    float64(img.Meta.Size) / (1024 * 1024),
			CreatedAt: img.Meta.CreatedAt,
		})
	}
	for _, img := range registryImages {
		source := img.Meta.Source
		if source == "" {
			source = "registry"
		}
		items = append(items, uiImageSummary{
			Tag:       img.Tag,
			Source:    source,
			Digest:    img.Meta.Digest,
			SizeMB:    float64(img.Meta.Size) / (1024 * 1024),
			CreatedAt: img.Meta.CreatedAt,
		})
	}

	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{"images": items})
}

func (s *uiServer) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	var req uiDeleteImageRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	tag := decodeTag(req.Tag)
	if tag == "" {
		writeAPIError(w, http.StatusBadRequest, "tag is required")
		return
	}

	store := image.NewStore("")
	localErr := store.Remove(tag)
	if localErr != nil && !errors.Is(localErr, image.ErrImageNotFound) {
		writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUIRemoveImage, localErr).Error())
		return
	}
	if localErr == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"tag": tag, "removed": true, "scope": "local"})
		return
	}

	if err := image.RemoveRegistryCache(tag, ""); err != nil {
		if errors.Is(err, image.ErrImageNotFound) {
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("image %s not found", tag))
			return
		}
		writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUIRemoveImage, err).Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"tag": tag, "removed": true, "scope": "registry"})
}

func (s *uiServer) handleListSandboxes(w http.ResponseWriter) {
	states, err := s.stateMgr.List()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}

	managedIDs := s.managedSandboxIDs()
	items := make([]uiSandboxSummary, 0, len(states))
	for _, vmState := range states {
		items = append(items, uiSandboxSummary{
			ID:        vmState.ID,
			PID:       vmState.PID,
			Status:    vmState.Status,
			Image:     vmState.Image,
			CreatedAt: vmState.CreatedAt,
			Managed:   managedIDs[vmState.ID],
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"sandboxes": items})
}

func (s *uiServer) handleStartSandbox(w http.ResponseWriter, r *http.Request) {
	var req uiStartSandboxRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	req.Image = strings.TrimSpace(req.Image)
	if req.Image == "" {
		writeAPIError(w, http.StatusBadRequest, "image is required")
		return
	}
	req.Workspace = strings.TrimSpace(req.Workspace)
	if req.Workspace != "" {
		if err := api.ValidateGuestMount(req.Workspace); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.CPUs < 0 || req.MemoryMB < 0 || req.DiskSizeMB < 0 {
		writeAPIError(w, http.StatusBadRequest, "cpus, memory_mb, and disk_size_mb must be >= 0")
		return
	}

	builder := image.NewBuilder(&image.BuildOptions{
		ForcePull: req.Pull,
	})
	result, err := builder.Build(r.Context(), req.Image)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUIBuildRootfs, err).Error())
		return
	}

	// Sandbox lifetime must outlive the HTTP request context. Keep a dedicated
	// context per managed VM and cancel it only on explicit stop/shutdown.
	vmCtx, vmCancel := context.WithCancel(context.Background())

	var imageCfg *api.ImageConfig
	if result.OCI != nil {
		imageCfg = &api.ImageConfig{
			User:       result.OCI.User,
			WorkingDir: result.OCI.WorkingDir,
			Entrypoint: result.OCI.Entrypoint,
			Cmd:        result.OCI.Cmd,
			Env:        result.OCI.Env,
		}
	}

	config := api.DefaultConfig()
	config.Image = req.Image
	config.Privileged = req.Privileged
	config.ImageCfg = imageCfg
	if req.CPUs > 0 {
		config.Resources.CPUs = req.CPUs
	}
	if req.MemoryMB > 0 {
		config.Resources.MemoryMB = req.MemoryMB
	}
	if req.DiskSizeMB > 0 {
		config.Resources.DiskSizeMB = req.DiskSizeMB
	}
	if req.Workspace != "" {
		config.VFS.Workspace = req.Workspace
		config.VFS.Mounts = map[string]api.MountConfig{
			req.Workspace: {Type: api.MountTypeMemory},
		}
	}

	sb, err := sandbox.New(vmCtx, config, &sandbox.Options{RootfsPath: result.RootfsPath})
	if err != nil {
		vmCancel()
		writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUICreateSandbox, err).Error())
		return
	}
	if err := sb.Start(vmCtx); err != nil {
		_ = sb.Close(vmCtx)
		_ = s.stateMgr.Remove(sb.ID())
		vmCancel()
		writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUIStartSandbox, err).Error())
		return
	}

	relay := sandbox.NewExecRelay(sb)
	execSocketPath := s.stateMgr.ExecSocketPath(sb.ID())
	if err := relay.Start(execSocketPath); err != nil {
		relay.Stop()
		_ = sb.Close(vmCtx)
		_ = s.stateMgr.Remove(sb.ID())
		vmCancel()
		writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUIStartExecRelay, err).Error())
		return
	}

	startupCommand := startupCommandFromImageConfig(imageCfg)
	if startupCommand != "" {
		sandboxID := sb.ID()
		go func() {
			// Stream to discard to avoid buffering long-running daemon logs in memory.
			opts := &api.ExecOptions{Stdout: io.Discard, Stderr: io.Discard}
			result, execErr := sb.Exec(vmCtx, startupCommand, opts)
			if execErr != nil {
				fmt.Fprintf(os.Stderr, "matchlock ui: sandbox %s startup command failed: %v\n", sandboxID, execErr)
				return
			}
			if result != nil && result.ExitCode != 0 {
				fmt.Fprintf(os.Stderr, "matchlock ui: sandbox %s startup command exited with code %d\n", sandboxID, result.ExitCode)
			}
		}()
	}

	s.mu.Lock()
	s.managed[sb.ID()] = &managedSandbox{
		sb:     sb,
		relay:  relay,
		cancel: vmCancel,
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":              sb.ID(),
		"image":           req.Image,
		"cached":          result.Cached,
		"digest":          result.Digest,
		"size_mb":         float64(result.Size) / (1024 * 1024),
		"startup_command": startupCommand,
	})
}

func (s *uiServer) handleSandboxActions(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/sandboxes/"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	sandboxID := parts[0]
	switch {
	case len(parts) == 2 && parts[1] == "stop":
		s.handleStopSandbox(w, r, sandboxID)
	case len(parts) == 2 && parts[1] == "rm":
		s.handleRemoveSandbox(w, r, sandboxID)
	case len(parts) == 3 && parts[1] == "terminal" && parts[2] == "ws":
		s.handleTerminalWebsocket(w, r, sandboxID)
	default:
		http.NotFound(w, r)
	}
}

func (s *uiServer) handleStopSandbox(w http.ResponseWriter, r *http.Request, sandboxID string) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.shutdownTimeout)
	defer cancel()

	err := s.stopManagedSandbox(ctx, sandboxID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id":      sandboxID,
			"status":  "stopped",
			"managed": true,
		})
		return
	case !errors.Is(err, errSandboxNotManaged):
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}

	st, err := s.stateMgr.Get(sandboxID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("sandbox %s not found", sandboxID))
		return
	}
	if st.Status != "running" {
		writeAPIError(w, http.StatusConflict, fmt.Sprintf("sandbox %s is not running (status: %s)", sandboxID, st.Status))
		return
	}
	if err := s.lifecycleMgr.Kill(sandboxID); err != nil {
		writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUIStopSandbox, err).Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":      sandboxID,
		"status":  "stopping",
		"managed": false,
	})
}

func (s *uiServer) handleRemoveSandbox(w http.ResponseWriter, r *http.Request, sandboxID string) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.shutdownTimeout)
	defer cancel()

	if err := s.stopManagedSandbox(ctx, sandboxID); err != nil && !errors.Is(err, errSandboxNotManaged) {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.lifecycleMgr.Remove(sandboxID); err != nil {
		writeAPIError(w, http.StatusConflict, errx.Wrap(ErrUIRemoveSandbox, err).Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":      sandboxID,
		"removed": true,
	})
}

func (s *uiServer) stopManagedSandbox(ctx context.Context, sandboxID string) error {
	s.mu.Lock()
	ms, ok := s.managed[sandboxID]
	if ok {
		delete(s.managed, sandboxID)
	}
	s.mu.Unlock()

	if !ok {
		return errSandboxNotManaged
	}
	if ms.cancel != nil {
		ms.cancel()
	}
	if ms.relay != nil {
		ms.relay.Stop()
	}
	if ms.sb != nil {
		if err := ms.sb.Close(ctx); err != nil {
			return errx.Wrap(ErrUIStopSandbox, err)
		}
	}
	return nil
}

func (s *uiServer) handlePullImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}

	var req uiPullImageRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	req.Image = strings.TrimSpace(req.Image)
	req.Tag = strings.TrimSpace(req.Tag)
	if req.Image == "" {
		writeAPIError(w, http.StatusBadRequest, "image is required")
		return
	}

	builder := image.NewBuilder(&image.BuildOptions{ForcePull: req.Force})
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	result, err := builder.Build(ctx, req.Image)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUIPullImage, err).Error())
		return
	}

	if req.Tag != "" {
		if err := builder.SaveTag(req.Tag, result); err != nil {
			writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUITagImage, err).Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"image":   req.Image,
		"tag":     req.Tag,
		"cached":  result.Cached,
		"digest":  result.Digest,
		"size_mb": float64(result.Size) / (1024 * 1024),
	})
}

func (s *uiServer) handleTerminalWebsocket(w http.ResponseWriter, r *http.Request, sandboxID string) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}

	websocket.Handler(func(conn *websocket.Conn) {
		s.serveTerminalConn(conn, sandboxID)
	}).ServeHTTP(w, r)
}

func (s *uiServer) serveTerminalConn(conn *websocket.Conn, sandboxID string) {
	defer conn.Close()

	query := conn.Request().URL.Query()
	command := strings.TrimSpace(query.Get("command"))
	if command == "" {
		command = "sh"
	}
	workdir := strings.TrimSpace(query.Get("workdir"))
	user := strings.TrimSpace(query.Get("user"))
	rows := parseQueryUint16(query.Get("rows"), defaultUITerminalRows)
	cols := parseQueryUint16(query.Get("cols"), defaultUITerminalCols)
	resizeCh := make(chan [2]uint16, 1)

	execSocketPath := s.stateMgr.ExecSocketPath(sandboxID)
	if _, err := os.Stat(execSocketPath); err != nil {
		_ = websocket.Message.Send(conn, []byte(fmt.Sprintf("[terminal unavailable] exec socket for %s not found", sandboxID)))
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	input := &wsInputReader{conn: conn, cancel: cancel, resizeCh: resizeCh}
	output := &wsOutputWriter{conn: conn, cancel: cancel}

	exitCode, err := sandbox.ExecInteractiveViaRelay(ctx, execSocketPath, command, workdir, user, rows, cols, input, output, resizeCh)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			_ = output.sendStatus(fmt.Sprintf("terminal error: %v", err))
		}
		return
	}
	_ = output.sendStatus(fmt.Sprintf("process exited with code %d", exitCode))
}

func (s *uiServer) managedSandboxIDs() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := make(map[string]bool, len(s.managed))
	for id := range s.managed {
		ids[id] = true
	}
	return ids
}

func (s *uiServer) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}

	assetPath := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	assetPath = strings.TrimPrefix(assetPath, "/")
	if assetPath == "" || assetPath == "." {
		assetPath = "index.html"
	}
	if strings.HasPrefix(assetPath, "api/") {
		http.NotFound(w, r)
		return
	}

	data, err := fs.ReadFile(s.assets, assetPath)
	if err != nil {
		assetPath = "index.html"
		data, err = fs.ReadFile(s.assets, assetPath)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, errx.Wrap(ErrUIServeAssets, err).Error())
			return
		}
	}

	if contentType := mime.TypeByExtension(path.Ext(assetPath)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

func decodeJSONBody(r *http.Request, dst interface{}) error {
	defer r.Body.Close()

	decoder := json.NewDecoder(io.LimitReader(r.Body, maxUIRequestBodySize))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errx.Wrap(ErrUIInvalidRequest, fmt.Errorf("request body is required"))
		}
		return errx.Wrap(ErrUIInvalidRequest, err)
	}

	var extra struct{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errx.Wrap(ErrUIInvalidRequest, fmt.Errorf("request body must contain a single JSON object"))
	}

	return nil
}

func writeJSON(w http.ResponseWriter, statusCode int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
}

func writeAPIError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]string{"error": message})
}

func writeMethodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func parseQueryUint16(value string, fallback uint16) uint16 {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 10, 16)
	if err != nil || parsed == 0 {
		return fallback
	}
	return uint16(parsed)
}

func startupCommandFromImageConfig(imageCfg *api.ImageConfig) string {
	if imageCfg == nil {
		return ""
	}
	composed := imageCfg.ComposeCommand(nil)
	if len(composed) == 0 {
		return ""
	}
	return api.ShellQuoteArgs(composed)
}

func decodeTag(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	decoded, err := url.QueryUnescape(trimmed)
	if err != nil {
		return trimmed
	}
	return strings.TrimSpace(decoded)
}

type wsInputReader struct {
	conn     *websocket.Conn
	cancel   context.CancelFunc
	buf      []byte
	resizeCh chan [2]uint16
}

func (r *wsInputReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		var msg []byte
		if err := websocket.Message.Receive(r.conn, &msg); err != nil {
			r.cancel()
			return 0, io.EOF
		}
		if len(msg) == 0 {
			continue
		}

		frameType := msg[0]
		payload := msg[1:]
		switch frameType {
		case wsFrameTypeInput:
			if len(payload) == 0 {
				continue
			}
			r.buf = append(r.buf[:0], payload...)
		case wsFrameTypeResize:
			if r.resizeCh != nil && len(payload) >= 4 {
				rows := binary.BigEndian.Uint16(payload[0:2])
				cols := binary.BigEndian.Uint16(payload[2:4])
				if rows > 0 && cols > 0 {
					enqueueResize(r.resizeCh, [2]uint16{rows, cols})
				}
			}
		default:
			// Backward compatibility for older UI clients that sent raw bytes.
			r.buf = append(r.buf[:0], msg...)
		}
	}

	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func enqueueResize(ch chan [2]uint16, size [2]uint16) {
	select {
	case ch <- size:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- size:
		default:
		}
	}
}

type wsOutputWriter struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
	mu     sync.Mutex
}

func (w *wsOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	payload := make([]byte, len(p))
	copy(payload, p)
	if err := websocket.Message.Send(w.conn, payload); err != nil {
		w.cancel()
		return 0, err
	}
	return len(p), nil
}

func (w *wsOutputWriter) sendStatus(message string) error {
	_, err := w.Write([]byte(fmt.Sprintf("\r\n[%s]\r\n", message)))
	return err
}
