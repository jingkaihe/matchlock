//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/sdk"
)

func TestWriteAndReadFile(t *testing.T) {
	t.Parallel()
	client := launchAlpineWithWorkspace(t)

	content := []byte("hello from acceptance test")
	err := client.WriteFile(context.Background(), "/workspace/test.txt", content)
	require.NoError(t, err, "WriteFile")

	got, err := client.ReadFile(context.Background(), "/workspace/test.txt")
	require.NoError(t, err, "ReadFile")
	assert.Equal(t, content, got)
}

func TestWriteFileAndExec(t *testing.T) {
	t.Parallel()
	client := launchAlpineWithWorkspace(t)

	script := []byte("#!/bin/sh\necho script-output\n")
	err := client.WriteFileMode(context.Background(), "/workspace/run.sh", script, 0755)
	require.NoError(t, err, "WriteFileMode")

	result, err := client.Exec(context.Background(), "sh /workspace/run.sh")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "script-output", strings.TrimSpace(result.Stdout))
}

func TestListFiles(t *testing.T) {
	t.Parallel()
	client := launchAlpineWithWorkspace(t)

	err := client.WriteFile(context.Background(), "/workspace/a.txt", []byte("a"))
	require.NoError(t, err, "WriteFile a")
	err = client.WriteFile(context.Background(), "/workspace/b.txt", []byte("bb"))
	require.NoError(t, err, "WriteFile b")

	files, err := client.ListFiles(context.Background(), "/workspace")
	require.NoError(t, err, "ListFiles")

	names := make(map[string]bool)
	for _, f := range files {
		names[f.Name] = true
	}
	assert.True(t, names["a.txt"] && names["b.txt"], "ListFiles = %v, want a.txt and b.txt present", names)
}

func TestLargeFileRoundtrip(t *testing.T) {
	t.Parallel()
	client := launchAlpineWithWorkspace(t)

	data := bytes.Repeat([]byte("abcdefghij"), 10000) // 100KB
	err := client.WriteFile(context.Background(), "/workspace/large.bin", data)
	require.NoError(t, err, "WriteFile")

	got, err := client.ReadFile(context.Background(), "/workspace/large.bin")
	require.NoError(t, err, "ReadFile")
	assert.Equal(t, data, got)
}

func TestChmodViaExec(t *testing.T) {
	t.Parallel()
	client := launchAlpineWithWorkspace(t)

	err := client.WriteFile(context.Background(), "/workspace/script.sh", []byte("#!/bin/sh\necho chmod-works\n"))
	require.NoError(t, err, "WriteFile")

	_, err = client.Exec(context.Background(), "chmod +x /workspace/script.sh")
	require.NoError(t, err, "chmod")

	result, err := client.Exec(context.Background(), "/workspace/script.sh")
	require.NoError(t, err, "Exec script")
	assert.Equal(t, "chmod-works", strings.TrimSpace(result.Stdout))
}

func TestChmodPreservesAfterStat(t *testing.T) {
	t.Parallel()
	client := launchAlpineWithWorkspace(t)

	err := client.WriteFile(context.Background(), "/workspace/check.sh", []byte("#!/bin/sh\necho ok\n"))
	require.NoError(t, err, "WriteFile")

	_, err = client.Exec(context.Background(), "chmod 755 /workspace/check.sh")
	require.NoError(t, err, "chmod")

	result, err := client.Exec(context.Background(), "stat -c '%a' /workspace/check.sh")
	require.NoError(t, err, "stat")
	assert.Equal(t, "755", strings.TrimSpace(result.Stdout))
}

func TestSDKCopyInAndOut(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)
	ctx := context.Background()
	src, blob := writeCopySource(t)

	require.NoError(t, client.CopyIn(ctx, src+"/.", "/workspace", nil), "CopyIn")
	result, err := client.Exec(ctx, "cat /workspace/sub/b.txt && echo built > /workspace/out.txt")
	require.NoError(t, err)
	assert.Equal(t, "world\n", result.Stdout)

	out := filepath.Join(t.TempDir(), "out")
	require.NoError(t, client.CopyOut(ctx, "/workspace", out, &sdk.CopyOutOptions{Exclude: []string{"sub"}}), "CopyOut")
	got, err := os.ReadFile(filepath.Join(out, "blob.bin"))
	require.NoError(t, err)
	assert.True(t, bytes.Equal(blob, got), "blob corrupted in round trip")
	assert.FileExists(t, filepath.Join(out, "out.txt"))
	assert.NoDirExists(t, filepath.Join(out, "sub"))
}
