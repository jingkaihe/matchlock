//go:build acceptance && linux

package acceptance

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/require"
)

// TestQEMUTAPReapedOnCrash proves the QEMU TAP-leak fix: the qm-* TAP must be
// reaped by the kernel in all teardown orders, and the changed teardownNetwork
// must not regress the clean-close path.
//
// It is QEMU-only: it derives a `qm-*` TAP name via tapForVMID and asserts on
// that interface, so it must run with MATCHLOCK_BACKEND=qemu. On the default
// KVM host the product selects Firecracker (which creates `fc-*` TAPs), so the
// qm-* name can never appear; the gate below skips with an explicit message
// instead of failing the launch. If qemu-system-<arch> is not installed, it
// also skips with the missing prerequisite named.
//
//   - clean: normal client.Close — the regression case for teardownNetwork, which
//     no longer calls DeleteInterface (that re-CREATES a vanished TAP by name).
//   - child_dies: SIGKILL the vmID-scoped QEMU PID first (the child), then the
//     rpc (QEMU's ppid). QEMU's dup drops first, then the parent's fd.
//   - parent_dies: SIGKILL the rpc parent first, then QEMU. The parent's fd drops
//     first, then QEMU's dup.
//
// Both crash orders end with ALL descriptors closed. With a NON-PERSISTENT TAP the
// kernel destroys the interface once the last descriptor closes — the exact
// property that fixes the orphaned-`qm-*` leak. It signals only PIDs it recovered
// for the launched vmID (QEMU via the vmID in argv, the rpc via QEMU's ppid).
func TestQEMUTAPReapedOnCrash(t *testing.T) {
	if run, reason := qemuBackendGate(os.Getenv("MATCHLOCK_BACKEND"), runtime.GOARCH, exec.LookPath); !run {
		t.Skip(reason)
	}
	for _, mode := range []string{"clean", "child_dies", "parent_dies"} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			client, err := sdk.NewClient(sdk.Config{BinaryPath: os.Getenv("MATCHLOCK_BIN")})
			require.NoError(t, err)
			defer func() { _ = client.Close(0); _ = client.Remove() }()

			builder := sdk.New("alpine:latest").WithCPUs(0.5).AllowHost("httpbin.org").AllowPrivateIPs()
			vmID, err := client.Launch(builder)
			require.NoError(t, err)
			t.Logf("launched vmID=%s mode=%s", vmID, mode)

			tap := tapForVMID(vmID)
			require.Eventually(t, func() bool { return tapExists(tap) }, 20*time.Second, 200*time.Millisecond,
				"TAP %s should appear once the guest starts", tap)
			require.True(t, tapExists(tap), "TAP %s present before end-of-test", tap)

			if mode == "clean" {
				// Regression: normal Close must reap the TAP (teardownNetwork now
				// closes the fd only — no DeleteInterface by name).
				require.NoError(t, client.Close(0))
				require.Eventually(t, func() bool { return !tapExists(tap) }, 20*time.Second, 250*time.Millisecond,
					"TAP %s must be reaped on clean Close", tap)
				require.False(t, tapExists(tap), "TAP %s leaked on clean Close", tap)
				t.Logf("clean-close reaped TAP %s", tap)
				return
			}

			qemuPID := qemuPIDForVMID(vmID)
			require.NotZero(t, qemuPID, "no QEMU process carrying vmID %s", vmID)
			rpcPID := rpcParentPID(qemuPID)
			require.NotZero(t, rpcPID, "no rpc parent for qemu pid %d", qemuPID)
			t.Logf("qemuPID=%d rpcPID=%d", qemuPID, rpcPID)

			// SIGKILL in the mode's order; no clean Close anywhere.
			if mode == "child_dies" {
				require.NoError(t, syscall.Kill(qemuPID, syscall.SIGKILL), "kill qemu pid %d", qemuPID)
				_ = syscall.Kill(rpcPID, syscall.SIGKILL)
			} else { // parent_dies
				require.NoError(t, syscall.Kill(rpcPID, syscall.SIGKILL), "kill rpc pid %d", rpcPID)
				_ = syscall.Kill(qemuPID, syscall.SIGKILL)
			}

			require.Eventually(t, func() bool { return !tapExists(tap) }, 30*time.Second, 250*time.Millisecond,
				"TAP %s must be reaped by the kernel after %s (no leak)", tap, mode)
			require.False(t, tapExists(tap), "TAP %s leaked after %s", tap, mode)
			t.Logf("%s reaped TAP %s (no leak)", mode, tap)
		})
	}
}

// tapForVMID is declared in ipv6_linklocal_test.go (same package); reused.

func tapExists(tap string) bool {
	_, err := os.Stat("/sys/class/net/" + tap)
	return err == nil
}

// qemuPIDForVMID returns the PID of the QEMU process whose argv carries vmID, or
// 0. QEMU receives the TAP by FD and carries the vmID in its -serial file: arg,
// so matching argv is exact and scoped to this sandbox.
func qemuPIDForVMID(vmID string) int {
	out, err := exec.Command("pgrep", "-f", "qemu-system").Output()
	if err != nil {
		return 0
	}
	for _, pid := range strings.Fields(string(out)) {
		argv, err := os.ReadFile("/proc/" + pid + "/cmdline")
		if err != nil {
			continue
		}
		if strings.Contains(strings.ReplaceAll(string(argv), "\x00", " "), vmID) {
			if n, err := strconv.Atoi(pid); err == nil {
				return n
			}
		}
	}
	return 0
}

// rpcParentPID returns the parent (rpc) PID of the given QEMU PID. The rpc is
// QEMU's direct parent; reading /proc/<qemuPID>/stat's ppid is exact and scoped
// to this sandbox, with no global pgrep and no CLI-table parsing. Returns 0 if it
// cannot be read (e.g. QEMU already reaped).
func rpcParentPID(qemuPID int) int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(qemuPID) + "/stat")
	if err != nil {
		return 0
	}
	// Format: "pid (comm) state ppid pgrp ...". comm may contain spaces and ')',
	// so split on the LAST ')' to isolate the fields after comm. After the last
	// ')', the next field is the process state (S/R/D...), and the field AFTER
	// that is the ppid. Example: "7960 (qemu-system-x86) S 8207 ..." -> ppid=8207.
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 || idx+3 >= len(s) {
		return 0
	}
	rest := strings.Fields(s[idx+1:]) // fields after the comm *)
	if len(rest) < 2 {                // need at least state + ppid
		return 0
	}
	// rest[0] = state, rest[1] = ppid
	if n, err := strconv.Atoi(rest[1]); err == nil {
		return n
	}
	return 0
}
