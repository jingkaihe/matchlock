//go:build linux

package qemu

import (
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/stretchr/testify/require"
)

func TestTapNameForVMID(t *testing.T) {
	require.Equal(t, "qm-abc12345", tapNameForVMID("vm-abc12345"))
	require.Equal(t, "qm-abcdef", tapNameForVMID("vm-abcdef"))         // short id
	require.Equal(t, "qm-12345678", tapNameForVMID("12345678"))        // no prefix
	require.Equal(t, "qm-12345678", tapNameForVMID("vm-1234567890ab")) // truncated to 8
}

func TestGenerateMAC(t *testing.T) {
	// Deterministic, locally-administered (AA:FC prefix), unicast.
	mac := generateMAC("qemu-qm-abc12345")
	require.True(t, strings.HasPrefix(mac, "AA:FC:"), "MAC should be locally administered: %s", mac)
	require.Equal(t, 17, len(mac), "MAC should be colon-separated 6 bytes")
	// Determinism
	require.Equal(t, mac, generateMAC("qemu-qm-abc12345"))
	// Different seeds -> different MACs (enough entropy to catch a bug).
	require.NotEqual(t, mac, generateMAC("qemu-qm-abc99999"))
}

func TestChildFDFor(t *testing.T) {
	require.Equal(t, 3, childFDFor(0))
	require.Equal(t, 4, childFDFor(1))
	require.Equal(t, 12, childFDFor(9))
}

func TestKernelIPDNSSuffix(t *testing.T) {
	require.Equal(t, "", kernelIPDNSSuffix(nil))
	require.Equal(t, ":8.8.8.8", kernelIPDNSSuffix([]string{"8.8.8.8"}))
	require.Equal(t, ":8.8.8.8:8.8.4.4", kernelIPDNSSuffix([]string{"8.8.8.8", "8.8.4.4"}))
	// ip= kernel param accepts at most two DNS servers; extra ones are dropped.
	require.Equal(t, ":1.1.1.1:8.8.8.8", kernelIPDNSSuffix([]string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}))
}

func TestNetworkBootArgs(t *testing.T) {
	m := &Machine{config: &vm.VMConfig{
		GuestIP:    "192.168.50.2",
		GatewayIP:  "192.168.50.1",
		MTU:        1400,
		DNSServers: []string{"1.1.1.1", "8.8.8.8"},
	}}
	got := m.networkBootArgs()
	require.Contains(t, got, "ip=192.168.50.2::192.168.50.1:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8")
	require.Contains(t, got, "matchlock.mtu=1400")
}

func TestNetworkBootArgsDefaults(t *testing.T) {
	m := &Machine{config: &vm.VMConfig{}}
	got := m.networkBootArgs()
	require.Contains(t, got, "ip=192.168.100.2::192.168.100.1:255.255.255.0::eth0:off")
	require.Contains(t, got, "matchlock.mtu=1500")
}

func TestEffectiveMTU(t *testing.T) {
	require.Equal(t, 1500, effectiveMTU(0))
	require.Equal(t, 9000, effectiveMTU(9000))
}

func TestNetworkArgsUsesPassedFD(t *testing.T) {
	n := &networkSetup{tapName: "qm-test"}
	args := n.networkArgs(0)
	require.Equal(t, []string{
		"-netdev", "tap,id=net0,fd=3,vhost=off",
		"-device", "virtio-net-pci,netdev=net0,mac=" + generateMAC("qemu-qm-test"),
	}, args)
	// fd index 1 (after cid lock) maps to child fd 4
	args2 := n.networkArgs(1)
	require.Contains(t, args2[1], "fd=4")
}
