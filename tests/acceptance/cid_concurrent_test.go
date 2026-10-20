//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/require"
)

// TestConcurrentGuestsIndependent verifies that two concurrently-launched sandboxes
// are independently usable. The discriminating oracle is two-phase:
//  1. Each guest writes a DISTINCT nonce to the SAME guest-local path.
//  2. Only after both writes complete, each guest reads the path back.
//  3. Each must read its OWN nonce.
//
// If both SDK clients resolved to the same guest (a vsock/CID collision), the two
// writes would hit the same file and the later read would return the other
// guest's nonce — so this catches cross-sandbox contamination. It complements the
// deterministic unit test (pkg/vm/qemu.TestReserveGuestCIDConcurrentDistinct).
func TestConcurrentGuestsIndependent(t *testing.T) {
	const n = 2
	const guestPath = "/tmp/cid-identity"
	sandbox := func() *sdk.SandboxBuilder {
		return sdk.New("alpine:latest").WithCPUs(acceptanceDefaultCPUs).AllowHost("httpbin.org")
	}

	clients := make([]*sdk.Client, n)
	vids := make([]string, n)

	// Register cleanup BEFORE launching so a launch failure cannot strand a guest.
	t.Cleanup(func() {
		for _, c := range clients {
			if c != nil {
				c.Close(0)
				c.Remove()
			}
		}
	})

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			client, err := sdk.NewClient(matchlockConfig(t))
			if err != nil {
				errs[idx] = fmt.Errorf("sandbox %d NewClient: %w", idx, err)
				return
			}
			clients[idx] = client
			if _, err := client.Launch(sandbox()); err != nil {
				errs[idx] = fmt.Errorf("sandbox %d Launch: %w", idx, err)
				return
			}
			vids[idx] = client.VMID()
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		require.NoErrorf(t, errs[i], "sandbox %d launch", i)
		require.NotEmpty(t, vids[i], "vm %d id", i)
	}
	require.NotEqual(t, vids[0], vids[1], "two sandboxes must have distinct VMIDs")

	// Per-run distinct nonces (not constant, so a repeat can't mask stale state).
	nonce0 := fmt.Sprintf("guest-A-nonce-%d", time.Now().UnixNano())
	nonce1 := fmt.Sprintf("guest-B-nonce-%d", time.Now().UnixNano())
	nonces := []string{nonce0, nonce1}

	// Phase 1: each guest writes its own nonce to the SAME path. This phase
	// intentionally does NOT read back — a read back within the same exec passes
	// even on a guest collision (it would just read what that same guest wrote).
	// The read back that discriminates is phase 2.
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		res, err := clients[i].Exec(ctx, fmt.Sprintf("sh -c 'echo %s >%s'", nonces[i], guestPath))
		cancel()
		require.NoErrorf(t, err, "sandbox %d write exec must boot + vsock-ready", i)
		require.Equal(t, 0, res.ExitCode, "sandbox %d write exit code (out=%q)", i, res.Stdout+res.Stderr)
	}

	// Phase 2: each guest reads the SAME path and must see its OWN nonce. If the
	// two SDK clients resolved to the same guest (a CID/vsock collision), both
	// writes hit the same path and a cross-read returns the other's nonce.
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		res, err := clients[i].Exec(ctx, fmt.Sprintf("sh -c 'cat %s'", guestPath))
		cancel()
		require.NoErrorf(t, err, "sandbox %d read exec", i)
		require.Equal(t, 0, res.ExitCode, "sandbox %d read exit code (out=%q)", i, res.Stdout+res.Stderr)
		require.Equal(t, nonces[i], strings.TrimSpace(res.Stdout),
			"sandbox %d must read back its OWN nonce (cross-guest contamination would return the other)", i)
	}
}
