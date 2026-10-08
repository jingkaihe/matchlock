//go:build linux

package linux

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func ifaceExists(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// testTAPName builds a unique, valid Linux interface name. IFNAMSIZ is 16 on
// Linux, so the usable name is at most 15 chars. "tlt" + 8 random hex chars
// (4 random bytes) = 11 chars; an optional suffix keeps it under the limit.
func testTAPName(suffix string) string {
	return "tlt" + randHex(4) + suffix
}

// requirePrivileged skips these tests unless run as root with an explicit opt-in.
// It is NOT a capability probe; it documents the real requirement (creating TAP
// devices needs CAP_NET_ADMIN, typically root) so a non-privileged dev host does
// not accidentally exercise kernel state.
func requirePrivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root (CAP_NET_ADMIN) to create TAP devices")
	}
	if os.Getenv("MATCHLOCK_PRIVILEGED_TESTS") != "1" {
		t.Skip("set MATCHLOCK_PRIVILEGED_TESTS=1 to run privileged kernel TAP tests")
	}
}

// TestTAPLifetimeProvesNonPersistentReap exercises the real ioctl path used by
// the QEMU TAP-leak fix. It is a low-level kernel test (creating a TAP device on
// the host), NOT running matchlock. Cases:
//
//  1. Non-persistent TAP survives closing ONE of two dup'd descriptors and is
//     reaped by the kernel after the LAST descriptor closes (the property that
//     stops a QEMU crash from leaking a qm-* TAP).
//  2. Persistent TAP (Firecracker / legacy path) survives all owning descriptor
//     closures and is removed by DeleteInterface.
//  3. DeleteInterface on an absent unique name returns without leaving an
//     interface behind.
//  4. Repeating DeleteInterface is idempotent (no residual interface).
func TestTAPLifetimeProvesNonPersistentReap(t *testing.T) {
	requirePrivileged(t)

	// --- Case 1: non-persistent TAP, dup descriptors ---
	npName := testTAPName("")
	fd, err := CreateNonPersistentTAP(npName)
	require.NoError(t, err, "CreateNonPersistentTAP")
	fd2, err := syscall.Dup(fd)
	require.NoError(t, err, "dup non-persistent fd")
	t.Cleanup(func() { _ = syscall.Close(fd2); _ = syscall.Close(fd) })

	require.True(t, ifaceExists(npName), "non-persistent TAP %s should exist while a descriptor is open", npName)

	// Closing ONE descriptor must NOT reap it (the dup still holds the object).
	require.NoError(t, syscall.Close(fd), "close one non-persistent fd")
	fd = -1 // mark closed to avoid double-close in cleanup
	require.True(t, ifaceExists(npName), "non-persistent TAP %s must survive closing one of two descriptors", npName)

	// Closing the LAST descriptor must reap it.
	require.NoError(t, syscall.Close(fd2), "close last non-persistent fd")
	fd2 = -1
	require.Eventually(t, func() bool { return !ifaceExists(npName) }, 5*time.Second, 50*time.Millisecond,
		"non-persistent TAP %s must be reaped after the LAST descriptor closes (no leak)", npName)
	require.False(t, ifaceExists(npName), "non-persistent TAP %s leaked after last fd close", npName)

	// --- Case 2: persistent TAP survives all fd closes, removed by DeleteInterface ---
	persist := testTAPName("p") // 3+8+1 = 12 chars
	persistFD, err := CreateTAP(persist)
	require.NoError(t, err, "CreateTAP (persistent)")
	persistDup, err := syscall.Dup(persistFD)
	require.NoError(t, err, "dup persistent fd")
	t.Cleanup(func() { _ = syscall.Close(persistDup); _ = syscall.Close(persistFD) })

	require.True(t, ifaceExists(persist), "persistent TAP %s should exist while a descriptor is open", persist)
	require.NoError(t, syscall.Close(persistFD), "close one persistent fd")
	persistFD = -1
	require.NoError(t, syscall.Close(persistDup), "close last persistent fd")
	persistDup = -1
	time.Sleep(300 * time.Millisecond)
	require.True(t, ifaceExists(persist), "persistent TAP %s must SURVIVE all descriptor closes (TUNSETPERSIST)", persist)

	require.NoError(t, DeleteInterface(persist), "DeleteInterface persistent TAP")
	require.Eventually(t, func() bool { return !ifaceExists(persist) }, 5*time.Second, 50*time.Millisecond,
		"persistent TAP %s must be removed by DeleteInterface", persist)

	// --- Case 3: DeleteInterface on an absent name leaves no interface ---
	absent := testTAPName("x") // 3+8+1 = 12 chars
	require.NoError(t, DeleteInterface(absent), "DeleteInterface on absent name must not error")
	require.False(t, ifaceExists(absent), "absent-name DeleteInterface must not leave an interface")

	// --- Case 4: idempotent repeated deletion ---
	require.NoError(t, DeleteInterface(absent), "repeated DeleteInterface must be a no-op")
	require.False(t, ifaceExists(absent), "repeated DeleteInterface must not leave an interface")
}
