// Matchlock Go Client Example
//
// A simple client for interacting with Matchlock sandbox via JSON-RPC.
//
// Usage:
//
//	go run examples/sandbox_client.go
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
)

// Request represents a JSON-RPC request
type Request struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
	ID      uint64      `json:"id"`
}

// Response represents a JSON-RPC response
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      *uint64         `json:"id,omitempty"`
}

// RPCError represents a JSON-RPC error
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

// ExecResult represents the result of command execution
type ExecResult struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	DurationMS int64
}

// FileInfo represents file metadata
type FileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"is_dir"`
}

// Client is a Matchlock JSON-RPC client
type Client struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	requestID atomic.Uint64
	vmID      string
}

// NewClient creates a new Matchlock client
func NewClient(matchlockPath string) (*Client, error) {
	cmd := exec.Command("sudo", matchlockPath, "--rpc")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start matchlock: %w", err)
	}

	return &Client{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}, nil
}

// sendRequest sends a JSON-RPC request and returns the result
func (c *Client) sendRequest(method string, params interface{}) (json.RawMessage, error) {
	id := c.requestID.Add(1)

	req := Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	if _, err := fmt.Fprintln(c.stdin, string(data)); err != nil {
		return nil, fmt.Errorf("failed to write request: %w", err)
	}

	// Read response (skip notifications)
	for {
		line, err := c.stdout.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, fmt.Errorf("failed to unmarshal response: %w", err)
		}

		// Skip notifications (no ID)
		if resp.ID == nil {
			continue
		}

		if *resp.ID != id {
			continue
		}

		if resp.Error != nil {
			return nil, resp.Error
		}

		return resp.Result, nil
	}
}

// CreateConfig holds configuration for creating a sandbox
type CreateConfig struct {
	Image          string
	CPUs           int
	MemoryMB       int
	TimeoutSeconds int
	AllowedHosts   []string
}

// Create creates and starts a new sandbox VM
func (c *Client) Create(cfg CreateConfig) (string, error) {
	if cfg.Image == "" {
		cfg.Image = "standard"
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}
	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = 512
	}
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = 300
	}

	params := map[string]interface{}{
		"image": cfg.Image,
		"resources": map[string]interface{}{
			"cpus":            cfg.CPUs,
			"memory_mb":       cfg.MemoryMB,
			"timeout_seconds": cfg.TimeoutSeconds,
		},
	}

	if len(cfg.AllowedHosts) > 0 {
		params["network"] = map[string]interface{}{
			"allowed_hosts":    cfg.AllowedHosts,
			"block_private_ips": true,
		}
	}

	result, err := c.sendRequest("create", params)
	if err != nil {
		return "", err
	}

	var createResult struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &createResult); err != nil {
		return "", fmt.Errorf("failed to parse create result: %w", err)
	}

	c.vmID = createResult.ID
	return c.vmID, nil
}

// Exec executes a command in the sandbox
func (c *Client) Exec(command string) (*ExecResult, error) {
	params := map[string]string{
		"command": command,
	}

	result, err := c.sendRequest("exec", params)
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
		return nil, fmt.Errorf("failed to parse exec result: %w", err)
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

// WriteFile writes a file to the sandbox
func (c *Client) WriteFile(path string, content []byte, mode uint32) error {
	if mode == 0 {
		mode = 0644
	}

	params := map[string]interface{}{
		"path":    path,
		"content": base64.StdEncoding.EncodeToString(content),
		"mode":    mode,
	}

	_, err := c.sendRequest("write_file", params)
	return err
}

// ReadFile reads a file from the sandbox
func (c *Client) ReadFile(path string) ([]byte, error) {
	params := map[string]string{
		"path": path,
	}

	result, err := c.sendRequest("read_file", params)
	if err != nil {
		return nil, err
	}

	var readResult struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result, &readResult); err != nil {
		return nil, fmt.Errorf("failed to parse read result: %w", err)
	}

	return base64.StdEncoding.DecodeString(readResult.Content)
}

// ListFiles lists files in a directory
func (c *Client) ListFiles(path string) ([]FileInfo, error) {
	params := map[string]string{
		"path": path,
	}

	result, err := c.sendRequest("list_files", params)
	if err != nil {
		return nil, err
	}

	var listResult struct {
		Files []FileInfo `json:"files"`
	}
	if err := json.Unmarshal(result, &listResult); err != nil {
		return nil, fmt.Errorf("failed to parse list result: %w", err)
	}

	return listResult.Files, nil
}

// Close closes the sandbox and cleans up
func (c *Client) Close() error {
	c.sendRequest("close", nil)
	c.stdin.Close()
	return c.cmd.Wait()
}

func main() {
	fmt.Println("=== Matchlock Go Client Example ===\n")

	// Create client - use ./bin/matchlock or MATCHLOCK_BIN env var
	matchlockBin := os.Getenv("MATCHLOCK_BIN")
	if matchlockBin == "" {
		matchlockBin = "./bin/matchlock"
	}
	client, err := NewClient(matchlockBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create client: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	// Create sandbox
	fmt.Println("Creating sandbox...")
	vmID, err := client.Create(CreateConfig{
		Image:    "standard",
		CPUs:     1,
		MemoryMB: 512,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create sandbox: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created VM: %s\n\n", vmID)

	// Execute a simple command
	fmt.Println("Running 'echo Hello from sandbox!'...")
	result, err := client.Exec("echo 'Hello from sandbox!'")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to exec: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  stdout: %s", result.Stdout)
	fmt.Printf("  exit_code: %d\n", result.ExitCode)
	fmt.Printf("  duration: %dms\n\n", result.DurationMS)

	// Write a script to the sandbox
	script := `#!/bin/sh
echo "Hello from script!"
echo "Current directory: $(pwd)"
echo "Files in /workspace:"
ls -la /workspace
`
	fmt.Println("Writing script to /workspace/test.sh...")
	if err := client.WriteFile("/workspace/test.sh", []byte(script), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write file: %v\n", err)
		os.Exit(1)
	}

	// Execute the script
	fmt.Println("Running the script...")
	result, err = client.Exec("sh /workspace/test.sh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to exec script: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  stdout:\n%s", result.Stdout)
	if result.Stderr != "" {
		fmt.Printf("  stderr: %s\n", result.Stderr)
	}
	fmt.Printf("  exit_code: %d\n\n", result.ExitCode)

	// List files
	fmt.Println("Listing /workspace...")
	files, err := client.ListFiles("/workspace")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list files: %v\n", err)
		os.Exit(1)
	}
	for _, f := range files {
		ftype := "file"
		if f.IsDir {
			ftype = "dir"
		}
		fmt.Printf("  %s (%s, %d bytes)\n", f.Name, ftype, f.Size)
	}
	fmt.Println()

	// Read the file back
	fmt.Println("Reading /workspace/test.sh back...")
	content, err := client.ReadFile("/workspace/test.sh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read file: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Content length: %d bytes\n", len(content))
	fmt.Printf("  First line: %s\n\n", string(content[:20]))

	// Test error handling
	fmt.Println("Testing command that fails...")
	result, err = client.Exec("exit 42")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Exec returned error: %v\n", err)
	} else {
		fmt.Printf("  exit_code: %d\n\n", result.ExitCode)
	}

	fmt.Println("Closing sandbox...")
	fmt.Println("Done!")
}
