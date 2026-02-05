// Matchlock Go SDK Example
//
// Usage: go run examples/go/main.go
//
// With secrets:
//
//	ANTHROPIC_API_KEY=sk-xxx go run examples/go/main.go
package main

import (
	"fmt"
	"os"

	"github.com/jingkaihe/matchlock/pkg/sdk"
)

func main() {
	cfg := sdk.DefaultConfig()
	if os.Getenv("MATCHLOCK_BIN") == "" {
		cfg.BinaryPath = "./bin/matchlock"
	}

	client, err := sdk.NewClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create client: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	// Example with secrets - the API key is replaced in HTTP requests to api.anthropic.com
	opts := sdk.CreateOptions{Image: "standard"}
	if apiKey := os.Getenv("ANTHROPIC_API_KEY"); apiKey != "" {
		opts.Secrets = []sdk.Secret{{
			Name:  "ANTHROPIC_API_KEY",
			Value: apiKey,
			Hosts: []string{"api.anthropic.com"},
		}}
	}

	vmID, _ := client.Create(opts)
	fmt.Printf("Created VM: %s\n", vmID)

	result, _ := client.Exec("echo 'Hello from sandbox!'")
	fmt.Printf("Output: %sExit: %d, Duration: %dms\n", result.Stdout, result.ExitCode, result.DurationMS)

	script := "#!/bin/sh\necho \"Files in /workspace:\"\nls -la /workspace\n"
	client.WriteFileMode("/workspace/test.sh", []byte(script), 0755)

	result, _ = client.Exec("sh /workspace/test.sh")
	fmt.Printf("Script output:\n%s", result.Stdout)

	files, _ := client.ListFiles("/workspace")
	fmt.Printf("Files: %d items\n", len(files))

	content, _ := client.ReadFile("/workspace/test.sh")
	fmt.Printf("Read back: %d bytes\n", len(content))

	result, _ = client.Exec("exit 42")
	fmt.Printf("Failed command exit code: %d\n", result.ExitCode)
}
