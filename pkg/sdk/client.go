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
//	    AddHost("api.internal", "10.0.0.10").
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
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// Client is a Matchlock JSON-RPC client.
// All methods are safe for concurrent use.
type Client struct {
	cmd        *exec.Cmd
	binaryPath string
	stdin      io.WriteCloser
	stdout     *bufio.Reader
	stderr     io.ReadCloser
	requestID  atomic.Uint64
	vmID       string
	mu         sync.Mutex // legacy — kept for Close()
	closed     bool

	// Concurrent request handling
	writeMu    sync.Mutex                 // serializes writes to stdin
	pendingMu  sync.Mutex                 // protects pending map
	pending    map[uint64]*pendingRequest // in-flight requests by ID
	readerOnce sync.Once                  // ensures reader goroutine starts once

	vfsHookMu      sync.RWMutex
	vfsHooks       []compiledVFSHook
	vfsMutateHooks []compiledVFSMutateHook
	vfsActionHooks []compiledVFSActionHook
	vfsHookActive  atomic.Bool

	networkHookMu       sync.RWMutex
	networkHooks        map[string]compiledNetworkHook
	networkHookSocket   string
	networkHookListener net.Listener
	networkHookTempDir  string
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
		cmd:        cmd,
		binaryPath: cfg.BinaryPath,
		stdin:      stdin,
		stdout:     bufio.NewReader(stdout),
		stderr:     stderr,
		pending:    make(map[uint64]*pendingRequest),
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

	c.setVFSHooks(nil, nil, nil)
	c.stopNetworkHookServer()

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
	out, err := c.runCLICommand("rm", c.vmID)
	if err != nil {
		return errx.With(ErrRemoveVM, " %s: %s: %w", c.vmID, out, err)
	}
	return nil
}

// VolumeInfo describes a named volume returned by volume CRUD operations.
