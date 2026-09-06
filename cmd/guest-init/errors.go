//go:build linux

package main

import "errors"

var (
	ErrReadCmdline        = errors.New("read cmdline")
	ErrMissingDNS         = errors.New("missing matchlock.dns")
	ErrInvalidMTU         = errors.New("invalid matchlock.mtu")
	ErrInvalidCPUs        = errors.New("invalid matchlock.cpus")
	ErrInvalidAddHost     = errors.New("invalid matchlock.add_host")
	ErrInvalidDiskMount   = errors.New("invalid matchlock.disk")
	ErrMountExtraDisk     = errors.New("mount extra disk")
	ErrInvalidOverlayCfg  = errors.New("invalid overlay root config")
	ErrOverlaySetup       = errors.New("setup overlay root")
	ErrWriteHostname      = errors.New("write hostname")
	ErrWriteHosts         = errors.New("write hosts")
	ErrWriteResolvConf    = errors.New("write resolv.conf")
	ErrBringUpInterface   = errors.New("bring up interface")
	ErrSetInterfaceMTU    = errors.New("set interface mtu")
	ErrStartGuestFused    = errors.New("start guest-fused")
	ErrWorkspaceMount     = errors.New("check workspace mount")
	ErrWorkspaceMountWait = errors.New("workspace mount timeout")
	ErrInvalidExactMount  = errors.New("invalid matchlock.exact.mounts")
	ErrExactMountPrep     = errors.New("prepare exact destination mount")
	ErrExecGuestAgent     = errors.New("exec guest-agent")

	// ErrInvalidIPv6Link rejects a malformed matchlock.ipv6= kernel cmdline
	// field: the host and guest must agree on the per-VM IPv6 link, so a value
	// that cannot be parsed fails the boot instead of silently leaving the guest
	// without a route.
	ErrInvalidIPv6Link = errors.New("invalid matchlock.ipv6")
	// ErrConfigureGuestIPv6 covers the netlink round trip that installs the
	// guest IPv6 address and its default route.
	ErrConfigureGuestIPv6 = errors.New("configure guest ipv6")
)
