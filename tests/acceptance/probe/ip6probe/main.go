//go:build linux

// Command ip6probe is a static IPv6/IPv4 guest-side nonce-exchange client used by
// TestSDKIPv6LinkLocalBypass. It is built at test time by the acceptance package
// (see ipv6_linklocal_test.go) so the test is self-contained and does not depend
// on a manually-staged MATCHLOCK_IP6PROBE binary.
//
// Usage: ip6probe <tcp6|tcp4> <targetAddr> <targetPort> <reqNonce>
//
// It dials the target (IPv6 link-local with %eth0 zone, or IPv4), sends
// "NONCE <reqNonce>", reads the "ACK <reqNonce> <hostNonce>" reply, and prints
// LOCAL/REMOTE addresses plus PROBE_RECV <hostNonce>. Exit 0 on success.
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) != 5 {
		fmt.Fprintf(os.Stderr, "usage: ip6probe <tcp6|tcp4> <targetAddr> <targetPort> <reqNonce>\n")
		return 2
	}
	network, target, port, reqNonce := os.Args[1], os.Args[2], os.Args[3], os.Args[4]

	dialer := &net.Dialer{Timeout: 8 * time.Second}
	if network == "tcp6" {
		guestLL := eth0LinkLocal()
		if guestLL == nil {
			fmt.Fprintf(os.Stderr, "ERR: no fe80:: on guest eth0\n")
			return 1
		}
		dialer.LocalAddr = &net.TCPAddr{IP: guestLL, Zone: "eth0"}
	} else if network != "tcp4" {
		fmt.Fprintf(os.Stderr, "ERR: unknown network %q\n", network)
		return 2
	}

	full := net.JoinHostPort(target, port)
	conn, err := dialer.Dial(network, full)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR dial %s: %v\n", full, err)
		return 1
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))

	if _, err := fmt.Fprintf(conn, "NONCE %s\n", reqNonce); err != nil {
		fmt.Fprintf(os.Stderr, "ERR write: %v\n", err)
		return 1
	}
	fmt.Printf("LOCAL %s\n", conn.LocalAddr())
	fmt.Printf("REMOTE %s\n", conn.RemoteAddr())

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR read: %v\n", err)
		return 1
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) != 3 || fields[0] != "ACK" || fields[1] != reqNonce {
		fmt.Fprintf(os.Stderr, "ERR malformed reply: %q\n", strings.TrimSpace(line))
		return 1
	}
	fmt.Printf("PROBE_RECV %s\n", fields[2])
	return 0
}

// eth0LinkLocal returns the first fe80:: address on eth0, or nil.
func eth0LinkLocal() net.IP {
	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		return nil
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP
		if ip.To4() == nil && ip.IsLinkLocalUnicast() {
			return ip
		}
	}
	return nil
}
