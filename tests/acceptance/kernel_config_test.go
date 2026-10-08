//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/internal/assets/qemu-kernel"
	"github.com/jingkaihe/matchlock/pkg/kernel"
	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func copyCurrentKernelToTemp(t *testing.T) string {
	t.Helper()

	// On the amd64 QEMU backend, a kernel file-ref must be a QEMU-bootable
	// bzImage. The generic `kernel.ResolveKernelPath` resolves the ELF vmlinux
	// (25MB, Firecracker/generic), which QEMU's -kernel cannot boot -> it exits
	// with status 1. When the QEMU backend is active (MATCHLOCK_BACKEND=qemu,
	// amd64), resolve the vendored QEMU bzImage (kernel-qemu) instead so the
	// file-ref actually boots.
	var src string
	if os.Getenv("MATCHLOCK_BACKEND") == "qemu" && runtime.GOARCH == "amd64" {
		p, err := qemukernel.Ensure()
		require.NoError(t, err, "qemukernel.Ensure")
		src = p
	} else {
		var err error
		src, err = kernel.ResolveKernelPath(context.Background())
		require.NoError(t, err)
	}

	data, err := os.ReadFile(src)
	require.NoErrorf(t, err, "read kernel from %s", src)

	dst := filepath.Join(t.TempDir(), "kernel")
	require.NoError(t, os.WriteFile(dst, data, 0644))
	return dst
}

func assertKernelMetadata(t *testing.T, vmID, expectedRef, expectedPath string) {
	t.Helper()

	getStdout, _, exitCode := runCLI(t, "get", vmID)
	require.Equal(t, 0, exitCode)

	var getOut struct {
		Config struct {
			Kernel map[string]interface{} `json:"kernel"`
		} `json:"config"`
	}
	require.NoError(t, json.Unmarshal([]byte(getStdout), &getOut))
	require.NotNil(t, getOut.Config.Kernel)
	assert.Equal(t, expectedRef, getOut.Config.Kernel["ref"])

	inspectStdout, _, exitCode := runCLI(t, "inspect", vmID)
	require.Equal(t, 0, exitCode)

	var inspectOut struct {
		Lifecycle struct {
			Resources struct {
				KernelRef  string `json:"kernel_ref"`
				KernelPath string `json:"kernel_path"`
			} `json:"resources"`
		} `json:"lifecycle"`
	}
	require.NoError(t, json.Unmarshal([]byte(inspectStdout), &inspectOut))
	assert.Equal(t, expectedRef, inspectOut.Lifecycle.Resources.KernelRef)
	assert.Equal(t, expectedPath, inspectOut.Lifecycle.Resources.KernelPath)
}

func createWithOptions(t *testing.T, opts sdk.CreateOptions) *sdk.Client {
	t.Helper()

	if opts.CPUs == 0 {
		opts.CPUs = acceptanceDefaultCPUs
	}

	client, err := sdk.NewClient(matchlockConfig(t))
	require.NoError(t, err, "NewClient")

	t.Cleanup(func() {
		_ = client.Close(0)
		_ = client.Remove()
	})

	type createResult struct {
		id  string
		err error
	}

	done := make(chan createResult, 1)
	go func() {
		id, createErr := client.Create(opts)
		done <- createResult{id: id, err: createErr}
	}()

	select {
	case result := <-done:
		require.NoError(t, result.err, "Create")
		require.NotEmpty(t, result.id)
	case <-time.After(launchTimeout):
		_ = client.Close(0)
		_ = client.Remove()
		require.FailNowf(t, "Create timed out", "image=%s timeout=%s", opts.Image, launchTimeout)
	}

	return client
}

func TestCLIRunWithKernelFileRefPersistsKernelMetadata(t *testing.T) {
	localKernel := copyCurrentKernelToTemp(t)

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"--kernel", "file://"+localKernel,
		"-d",
	)
	require.Equal(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	vmID := strings.TrimSpace(stdout)
	require.NotEmpty(t, vmID, "stdout: %q stderr: %q", stdout, stderr)
	require.True(t, strings.HasPrefix(vmID, "vm-"), "stdout: %q stderr: %q", stdout, stderr)

	t.Cleanup(func() {
		_, _, _ = runCLI(t, "kill", vmID)
		_, _, _ = runCLI(t, "rm", vmID)
	})

	waitForDetachedVMExecReady(t, vmID)
	cmdOut, _, execExit := runCLIWithTimeout(t, 30*time.Second, "exec", vmID, "echo", "kernel-file-ref")
	require.Equal(t, 0, execExit)
	assert.Equal(t, "kernel-file-ref", strings.TrimSpace(cmdOut))
	assertKernelMetadata(t, vmID, "file://"+localKernel, localKernel)
}

func TestCLIRunRejectsRelativeKernelFileRef(t *testing.T) {
	_, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"--kernel", "file://kernel",
		"--",
		"true",
	)
	require.NotEqual(t, 0, exitCode)
	assert.Contains(t, stderr, "invalid kernel reference")
	assert.Contains(t, stderr, "file ref")
}

func waitForDetachedVMExecReady(t *testing.T, vmID string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, _, execExit := runCLIWithTimeout(t, 10*time.Second, "exec", vmID, "true")
		if execExit == 0 {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	require.FailNowf(t, "timed out waiting for detached sandbox exec readiness", "vmID=%s", vmID)
}

func TestGoSDKWithKernelPersistsInStateAndLifecycle(t *testing.T) {
	t.Parallel()

	localKernel := copyCurrentKernelToTemp(t)
	client := launchWithBuilder(t, sdk.New("alpine:latest").WithKernel("file://"+localKernel))
	vmID := client.VMID()
	require.NotEmpty(t, vmID)

	result, err := client.Exec(context.Background(), "echo sdk-kernel-ref")
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "sdk-kernel-ref", strings.TrimSpace(result.Stdout))
	assertKernelMetadata(t, vmID, "file://"+localKernel, localKernel)
}

func TestGoSDKCreateOptionsKernelRefPersistsInStateAndLifecycle(t *testing.T) {
	t.Parallel()

	localKernel := copyCurrentKernelToTemp(t)
	client := createWithOptions(t, sdk.CreateOptions{
		Image:     "alpine:latest",
		KernelRef: "file://" + localKernel,
	})
	vmID := client.VMID()
	require.NotEmpty(t, vmID)

	result, err := client.Exec(context.Background(), "echo sdk-create-kernel-ref")
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "sdk-create-kernel-ref", strings.TrimSpace(result.Stdout))
	assertKernelMetadata(t, vmID, "file://"+localKernel, localKernel)
}

func TestGoSDKCreateOptionsRejectsRelativeKernelFileRef(t *testing.T) {
	t.Parallel()

	client, err := sdk.NewClient(matchlockConfig(t))
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() {
		_ = client.Close(0)
		_ = client.Remove()
	})

	_, err = client.Create(sdk.CreateOptions{
		Image:     "alpine:latest",
		CPUs:      acceptanceDefaultCPUs,
		KernelRef: "file://kernel",
	})
	require.Error(t, err)

	var rpcErr *sdk.RPCError
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, sdk.ErrCodeVMFailed, rpcErr.Code)
	assert.Contains(t, rpcErr.Message, "invalid kernel reference")
	assert.Contains(t, rpcErr.Message, "file ref")
}
