// Package sdk provides a client for interacting with Matchlock sandboxes via JSON-RPC.
//
// Use the builder API for a fluent experience:
//
//	client, err := sdk.NewClient(sdk.DefaultConfig())
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close(0)
//
//	sandbox := sdk.New("python:3.12-alpine").
//	    WithCPUs(2).
//	    WithMemory(1024).
//	    AllowHost("dl-cdn.alpinelinux.org", "api.openai.com").
//	    AddSecret("API_KEY", os.Getenv("API_KEY"), "api.openai.com")
//
//	vmID, err := client.Launch(sandbox)
//
//	result, err := client.Exec(ctx, "echo hello")
//	fmt.Println(result.Stdout)
package sdk

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"

	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
)

// Client is a Matchlock JSON-RPC client.
// All methods are safe for concurrent use.
type Client struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	stderr    io.ReadCloser
	requestID atomic.Uint64
	vmID      string
	mu        sync.Mutex // legacy — kept for Close()
	closed    bool

	// Concurrent request handling
	writeMu    sync.Mutex                 // serializes writes to stdin
	pendingMu  sync.Mutex                 // protects pending map
	pending    map[uint64]*pendingRequest // in-flight requests by ID
	readerOnce sync.Once                  // ensures reader goroutine starts once
}

// Config holds client configuration
type Config struct {
	// BinaryPath is the path to the matchlock binary
	BinaryPath string
	// UseSudo runs matchlock with sudo (required for TAP devices)
	UseSudo bool
}

// DefaultConfig returns the default client configuration
func DefaultConfig() Config {
	path := os.Getenv("MATCHLOCK_BIN")
	if path == "" {
		path = "matchlock"
	}
	return Config{
		BinaryPath: path,
	}
}

// NewClient creates a new Matchlock client and starts the RPC process
func NewClient(cfg Config) (*Client, error) {
	var cmd *exec.Cmd
	if cfg.UseSudo {
		cmd = exec.Command("sudo", cfg.BinaryPath, "rpc")
	} else {
		cmd = exec.Command(cfg.BinaryPath, "rpc")
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errx.Wrap(ErrStdinPipe, err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errx.Wrap(ErrStdoutPipe, err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, errx.Wrap(ErrStderrPipe, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, errx.Wrap(ErrStartProc, err)
	}

	// Drain stderr in background to prevent blocking
	go io.Copy(io.Discard, stderr)

	return &Client{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		stderr:  stderr,
		pending: make(map[uint64]*pendingRequest),
	}, nil
}

// VMID returns the ID of the current VM, or empty string if none created
func (c *Client) VMID() string {
	return c.vmID
}

// Close closes the sandbox and cleans up resources.
// The VM state directory is preserved so it appears in "matchlock list".
// Call Remove after Close to delete the state entirely.
//
// timeout controls how long to wait for the process to exit after sending the
// close request. A zero value uses a short grace period and then force-kills
// if needed. When a non-zero timeout expires, the process is forcefully killed.
func (c *Client) Close(timeout time.Duration) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	effectiveTimeout := timeout
	if effectiveTimeout <= 0 {
		effectiveTimeout = 2 * time.Second
	}

	params := map[string]interface{}{
		"timeout_seconds": effectiveTimeout.Seconds(),
	}

	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()

	// Send close RPC with a bounded context so it doesn't block forever
	// (e.g. if the handler is draining in-flight cancelled requests).
	closeCtx, closeCancel := context.WithTimeout(context.Background(), effectiveTimeout+5*time.Second)
	c.sendRequestCtx(closeCtx, "close", params, nil)
	closeCancel()
	c.stdin.Close()

	select {
	case err := <-done:
		return err
	case <-time.After(effectiveTimeout):
		c.cmd.Process.Kill()
		<-done
		return errx.With(ErrCloseTimeout, " after %s", effectiveTimeout)
	}
}

// Remove deletes the stopped VM state directory.
// Must be called after Close. Uses the matchlock CLI binary
// that was configured in Config.BinaryPath.
func (c *Client) Remove() error {
	if c.vmID == "" {
		return nil
	}
	bin := c.cmd.Path
	out, err := exec.Command(bin, "rm", c.vmID).CombinedOutput()
	if err != nil {
		return errx.With(ErrRemoveVM, " %s: %s: %w", c.vmID, out, err)
	}
	return nil
}

// CreateOptions holds options for creating a sandbox
type CreateOptions struct {
	// Image is the container image reference (required, e.g., alpine:latest)
	Image string
	// Privileged skips in-guest security restrictions (seccomp, cap drop, no_new_privs)
	Privileged bool
	// CPUs is the number of vCPUs
	CPUs int
	// MemoryMB is the memory in megabytes
	MemoryMB int
	// DiskSizeMB is the disk size in megabytes (default: 5120)
	DiskSizeMB int
	// TimeoutSeconds is the maximum execution time
	TimeoutSeconds int
	// AllowedHosts is a list of allowed network hosts (supports wildcards)
	AllowedHosts []string
	// BlockPrivateIPs blocks access to private IP ranges
	BlockPrivateIPs bool
	// Mounts defines VFS mount configurations
	Mounts map[string]MountConfig
	// Env defines non-secret environment variables for command execution.
	// These are visible in VM state and inspect/get outputs.
	Env map[string]string
	// Secrets defines secrets to inject (replaced in HTTP requests to allowed hosts)
	Secrets []Secret
	// Workspace is the mount point for VFS in the guest (default: /workspace)
	Workspace string
	// DNSServers overrides the default DNS servers (8.8.8.8, 8.8.4.4)
	DNSServers []string
	// ImageConfig holds OCI image metadata (USER, ENTRYPOINT, CMD, WORKDIR, ENV)
	ImageConfig *ImageConfig
}

// ImageConfig holds OCI image metadata for user/entrypoint/cmd/workdir/env.
type ImageConfig struct {
	User       string            `json:"user,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// Secret defines a secret that will be injected as a placeholder env var
// and replaced with the real value in HTTP requests to allowed hosts
type Secret struct {
	// Name is the environment variable name (e.g., "ANTHROPIC_API_KEY")
	Name string
	// Value is the actual secret value
	Value string
	// Hosts is a list of hosts where this secret can be used (supports wildcards)
	Hosts []string
}

// MountConfig defines a VFS mount
type MountConfig struct {
	Type     string `json:"type"` // memory, real_fs, overlay
	HostPath string `json:"host_path,omitempty"`
	Readonly bool   `json:"readonly,omitempty"`
}

// Create creates and starts a new sandbox VM
func (c *Client) Create(opts CreateOptions) (string, error) {
	if opts.Image == "" {
		return "", ErrImageRequired
	}
	if opts.CPUs == 0 {
		opts.CPUs = api.DefaultCPUs
	}
	if opts.MemoryMB == 0 {
		opts.MemoryMB = api.DefaultMemoryMB
	}
	if opts.DiskSizeMB == 0 {
		opts.DiskSizeMB = api.DefaultDiskSizeMB
	}
	if opts.TimeoutSeconds == 0 {
		opts.TimeoutSeconds = api.DefaultTimeoutSeconds
	}

	params := map[string]interface{}{
		"image": opts.Image,
		"resources": map[string]interface{}{
			"cpus":            opts.CPUs,
			"memory_mb":       opts.MemoryMB,
			"disk_size_mb":    opts.DiskSizeMB,
			"timeout_seconds": opts.TimeoutSeconds,
		},
	}

	if opts.Privileged {
		params["privileged"] = true
	}

	if len(opts.AllowedHosts) > 0 || opts.BlockPrivateIPs || len(opts.Secrets) > 0 || len(opts.DNSServers) > 0 {
		network := map[string]interface{}{
			"allowed_hosts":     opts.AllowedHosts,
			"block_private_ips": opts.BlockPrivateIPs,
		}
		if len(opts.Secrets) > 0 {
			secrets := make(map[string]interface{})
			for _, s := range opts.Secrets {
				secrets[s.Name] = map[string]interface{}{
					"value": s.Value,
					"hosts": s.Hosts,
				}
			}
			network["secrets"] = secrets
		}
		if len(opts.DNSServers) > 0 {
			network["dns_servers"] = opts.DNSServers
		}
		params["network"] = network
	}

	if len(opts.Mounts) > 0 || opts.Workspace != "" {
		vfs := make(map[string]interface{})
		if len(opts.Mounts) > 0 {
			vfs["mounts"] = opts.Mounts
		}
		if opts.Workspace != "" {
			vfs["workspace"] = opts.Workspace
		}
		params["vfs"] = vfs
	}

	if len(opts.Env) > 0 {
		params["env"] = opts.Env
	}

	if opts.ImageConfig != nil {
		params["image_config"] = opts.ImageConfig
	}

	result, err := c.sendRequest("create", params)
	if err != nil {
		return "", err
	}

	var createResult struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &createResult); err != nil {
		return "", errx.Wrap(ErrParseCreateResult, err)
	}

	c.vmID = createResult.ID
	return c.vmID, nil
}

// ExecResult holds the result of command execution
type ExecResult struct {
	// ExitCode is the command's exit code
	ExitCode int
	// Stdout is the standard output
	Stdout string
	// Stderr is the standard error
	Stderr string
	// DurationMS is the execution time in milliseconds
	DurationMS int64
}

// Exec executes a command in the sandbox and returns the buffered result.
// The context controls the lifetime of the request — if cancelled, a cancel
// RPC is sent to abort the in-flight execution.
func (c *Client) Exec(ctx context.Context, command string) (*ExecResult, error) {
	return c.ExecWithDir(ctx, command, "")
}

// ExecWithDir executes a command in the sandbox with a working directory.
func (c *Client) ExecWithDir(ctx context.Context, command, workingDir string) (*ExecResult, error) {
	params := map[string]string{
		"command": command,
	}
	if workingDir != "" {
		params["working_dir"] = workingDir
	}

	result, err := c.sendRequestCtx(ctx, "exec", params, nil)
	if err != nil {
		return nil, err
	}

	var execResult struct {
		ExitCode   int    `json:"exit_code"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		DurationMS int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(result, &execResult); err != nil {
		return nil, errx.Wrap(ErrParseExecResult, err)
	}

	stdout, _ := base64.StdEncoding.DecodeString(execResult.Stdout)
	stderr, _ := base64.StdEncoding.DecodeString(execResult.Stderr)

	return &ExecResult{
		ExitCode:   execResult.ExitCode,
		Stdout:     string(stdout),
		Stderr:     string(stderr),
		DurationMS: execResult.DurationMS,
	}, nil
}

// ExecStreamResult holds the final result of a streaming exec (no stdout/stderr
// since those were delivered via the callback).
type ExecStreamResult struct {
	ExitCode   int
	DurationMS int64
}

// ExecStream executes a command and streams stdout/stderr to the provided writers
// in real-time. If stdout or stderr is nil, that stream is discarded.
// The final ExecStreamResult contains only the exit code and duration.
func (c *Client) ExecStream(ctx context.Context, command string, stdout, stderr io.Writer) (*ExecStreamResult, error) {
	return c.ExecStreamWithDir(ctx, command, "", stdout, stderr)
}

// ExecStreamWithDir executes a command with a working directory and streams
// stdout/stderr to the provided writers in real-time.
func (c *Client) ExecStreamWithDir(ctx context.Context, command, workingDir string, stdout, stderr io.Writer) (*ExecStreamResult, error) {
	params := map[string]string{
		"command": command,
	}
	if workingDir != "" {
		params["working_dir"] = workingDir
	}

	onNotification := func(method string, params json.RawMessage) {
		var chunk struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(params, &chunk); err != nil {
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			return
		}
		switch method {
		case "exec_stream.stdout":
			if stdout != nil {
				stdout.Write(decoded)
			}
		case "exec_stream.stderr":
			if stderr != nil {
				stderr.Write(decoded)
			}
		}
	}

	result, err := c.sendRequestCtx(ctx, "exec_stream", params, onNotification)
	if err != nil {
		return nil, err
	}

	var streamResult struct {
		ExitCode   int   `json:"exit_code"`
		DurationMS int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(result, &streamResult); err != nil {
		return nil, errx.Wrap(ErrParseExecStreamResult, err)
	}

	return &ExecStreamResult{
		ExitCode:   streamResult.ExitCode,
		DurationMS: streamResult.DurationMS,
	}, nil
}

// WriteFile writes content to a file in the sandbox.
func (c *Client) WriteFile(ctx context.Context, path string, content []byte) error {
	return c.WriteFileMode(ctx, path, content, 0644)
}

// WriteFileMode writes content to a file with specific permissions.
func (c *Client) WriteFileMode(ctx context.Context, path string, content []byte, mode uint32) error {
	params := map[string]interface{}{
		"path":    path,
		"content": base64.StdEncoding.EncodeToString(content),
		"mode":    mode,
	}

	_, err := c.sendRequestCtx(ctx, "write_file", params, nil)
	return err
}

// ReadFile reads a file from the sandbox.
func (c *Client) ReadFile(ctx context.Context, path string) ([]byte, error) {
	params := map[string]string{
		"path": path,
	}

	result, err := c.sendRequestCtx(ctx, "read_file", params, nil)
	if err != nil {
		return nil, err
	}

	var readResult struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result, &readResult); err != nil {
		return nil, errx.Wrap(ErrParseReadResult, err)
	}

	return base64.StdEncoding.DecodeString(readResult.Content)
}

// FileInfo holds file metadata
type FileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"is_dir"`
}

// ListFiles lists files in a directory.
func (c *Client) ListFiles(ctx context.Context, path string) ([]FileInfo, error) {
	params := map[string]string{
		"path": path,
	}

	result, err := c.sendRequestCtx(ctx, "list_files", params, nil)
	if err != nil {
		return nil, err
	}

	var listResult struct {
		Files []FileInfo `json:"files"`
	}
	if err := json.Unmarshal(result, &listResult); err != nil {
		return nil, errx.Wrap(ErrParseListResult, err)
	}

	return listResult.Files, nil
}
