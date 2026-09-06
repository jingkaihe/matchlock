//go:build linux

package sandbox

import (
	"errors"
	"net"
	"strconv"
	"testing"

	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/stretchr/testify/require"
)

// leasedSubnetInfo is the shape pkg/state hands out for a VM: an IPv4 /24 plus
// the matching per-VM IPv6 unique-local /64 (see state.ipv6ForOctet).
func leasedSubnetInfo(octet int) *state.SubnetInfo {
	return &state.SubnetInfo{
		Octet:       octet,
		GatewayIP:   "192.168.100.1",
		GuestIP:     "192.168.100.2",
		Subnet:      "192.168.100.0/24",
		GatewayIPv6: "fd00:100::1",
		GuestIPv6:   "fd00:100::2",
		Subnet6:     "fd00:100::/64",
		VMID:        "vm-wiring",
	}
}

// recordingDeps captures the addressing the sandbox hands to the proxy and DNS
// constructors while still returning live objects: a real proxy bound on
// loopback (so the ports the ip6 table redirects to are the ports the proxy
// actually reported) and a real forwarder. The IPv6 bind addresses are recorded
// rather than bound, because the guest's gateway address does not exist on the
// host.
type recordingDeps struct {
	proxyCfg  *sandboxnet.ProxyConfig
	dnsV4Addr string
	dnsV6Addr string
	proxy     *sandboxnet.TransparentProxy
}

func (r *recordingDeps) deps() interceptionDeps {
	return interceptionDeps{
		newProxy: func(cfg *sandboxnet.ProxyConfig) (*sandboxnet.TransparentProxy, error) {
			r.proxyCfg = cfg
			c := *cfg
			c.BindAddr = "127.0.0.1"
			c.BindAddrV6 = ""
			p, err := sandboxnet.NewTransparentProxy(&c)
			r.proxy = p
			return p, err
		},
		newDNS: func(bindAddrV4, bindAddrV6 string, dnsServers []string) (*sandboxnet.DNSForwarder, error) {
			r.dnsV4Addr, r.dnsV6Addr = bindAddrV4, bindAddrV6
			return sandboxnet.NewDNSForwarder("127.0.0.1", dnsServers)
		},
	}
}

// TestInterceptionWiringWiresIPv6ThroughEveryConsumer asserts the whole wiring
// for an intercepted sandbox: the lease's IPv6 gateway/guest reach the backend
// config, the proxy bind address, the DNS forwarder bind address and the ip6
// table, whose redirects target the ports those listeners reported.
func TestInterceptionWiringWiresIPv6ThroughEveryConsumer(t *testing.T) {
	w := newInterceptionWiring(leasedSubnetInfo(100), true)
	require.True(t, w.interceptionEnabled())
	w.tapName = "fc-wiring1"

	// The backend config carries the guest's IPv6 addressing; the TAP address
	// and the guest's matchlock.ipv6= boot arg are both derived from these
	// fields by the VM backend.
	vmCfg := &vm.VMConfig{}
	w.applyToVMConfig(vmCfg)
	require.Equal(t, "192.168.100.1", vmCfg.GatewayIP)
	require.Equal(t, "192.168.100.2", vmCfg.GuestIP)
	require.Equal(t, "192.168.100.1/24", vmCfg.SubnetCIDR)
	require.Equal(t, "fd00:100::1", vmCfg.GatewayIPv6)
	require.Equal(t, "fd00:100::2", vmCfg.GuestIPv6)
	require.Equal(t, "fd00:100::/64", vmCfg.Subnet6CIDR)

	rec := &recordingDeps{}
	proxy, dnsForwarder, rules, err := provisionInterception(rec.deps(), w, interceptionInputs{dnsServers: []string{"127.0.0.53"}})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, proxy.Close())
		require.NoError(t, dnsForwarder.Close())
	})

	// The proxy and the DNS forwarder both listen on the guest's IPv6 gateway:
	// that address is the ip6 DNAT target.
	require.Equal(t, "192.168.100.1", rec.proxyCfg.BindAddr)
	require.Equal(t, "fd00:100::1", rec.proxyCfg.BindAddrV6)
	require.Equal(t, "192.168.100.1", rec.dnsV4Addr)
	require.Equal(t, "fd00:100::1", rec.dnsV6Addr)

	// The per-TAP ip6 table is pointed at that gateway and at the very ports
	// the proxy and forwarder reported, which is what makes the ip6 redirect
	// reach them.
	require.Equal(t, "matchlock6_fc-wiring1", rules.TableNameV6())
	require.Equal(t, "fd00:100::1", rules.GatewayIPv6().String())
	httpPort, httpsPort, passPort, dnsPort := rules.InterceptionPorts()
	require.Equal(t, proxy.HTTPPort(), httpPort)
	require.Equal(t, proxy.HTTPSPort(), httpsPort)
	require.Equal(t, proxy.PassthroughPort(), passPort)
	require.Equal(t, dnsForwarder.Port(), dnsPort)
	require.NotZero(t, httpPort)
	require.NotZero(t, dnsPort)
	// The lifecycle record names the same table the rules install.
	require.Equal(t, "matchlock6_fc-wiring1", w.firewallTableV6())
}

// TestInterceptionWiringIPv6OffIsUnchanged pins the two shapes that must not
// gain any IPv6 configuration: a non-interception sandbox (plain NAT) and a
// --no-network sandbox.
func TestInterceptionWiringIPv6OffIsUnchanged(t *testing.T) {
	t.Run("plain NAT sandbox", func(t *testing.T) {
		w := newInterceptionWiring(leasedSubnetInfo(101), false)
		require.False(t, w.interceptionEnabled())
		require.Empty(t, w.gatewayIPv6)
		require.Empty(t, w.guestIPv6)
		require.Empty(t, w.subnet6CIDR)
		require.Empty(t, w.firewallTableV6())

		vmCfg := &vm.VMConfig{}
		w.applyToVMConfig(vmCfg)
		// IPv4 is provisioned exactly as before...
		require.Equal(t, "192.168.100.1", vmCfg.GatewayIP)
		require.Equal(t, "192.168.100.2", vmCfg.GuestIP)
		require.Equal(t, "192.168.100.1/24", vmCfg.SubnetCIDR)
		// ...and no IPv6 field is set, so the backend config (TAP address and
		// kernel args) is byte-identical to the IPv4-only behaviour.
		require.Empty(t, vmCfg.GatewayIPv6)
		require.Empty(t, vmCfg.GuestIPv6)
		require.Empty(t, vmCfg.Subnet6CIDR)
	})

	t.Run("no-network sandbox", func(t *testing.T) {
		w := newInterceptionWiring(nil, true)
		require.False(t, w.interceptionEnabled())
		require.Empty(t, w.gatewayIPv4)
		require.Empty(t, w.gatewayIPv6)
		require.Empty(t, w.firewallTableV6())

		vmCfg := &vm.VMConfig{}
		w.applyToVMConfig(vmCfg)
		require.Empty(t, vmCfg.GatewayIP)
		require.Empty(t, vmCfg.GatewayIPv6)
		require.Empty(t, vmCfg.Subnet6CIDR)
	})
}

// TestProvisionInterceptionFailureReleasesListeners covers the failure path
// after the listeners exist: when the DNS forwarder cannot be created (its own
// IPv6 bind failed), the proxy - including its IPv6 listeners - is closed again,
// so nothing is left bound for the caller's subnet release.
func TestProvisionInterceptionFailureReleasesListeners(t *testing.T) {
	w := newInterceptionWiring(leasedSubnetInfo(102), true)
	w.tapName = "fc-wiring2"
	require.True(t, w.interceptionEnabled())

	rec := &recordingDeps{}
	deps := rec.deps()
	deps.newDNS = func(bindAddrV4, bindAddrV6 string, dnsServers []string) (*sandboxnet.DNSForwarder, error) {
		require.Equal(t, "fd00:100::1", bindAddrV6)
		return nil, errors.New("bind [fd00:100::1]:5353: cannot assign requested address")
	}

	proxy, dnsForwarder, rules, err := provisionInterception(deps, w, interceptionInputs{})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCreateProxy)
	require.Nil(t, proxy)
	require.Nil(t, dnsForwarder)
	require.Nil(t, rules)

	// The proxy the sandbox had already started must be closed again: its HTTP
	// port is bindable.
	require.NotNil(t, rec.proxy)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(rec.proxy.HTTPPort()))
	ln, listenErr := net.Listen("tcp", addr)
	require.NoError(t, listenErr, "the proxy's listener must be released on the failure path")
	require.NoError(t, ln.Close())
}
