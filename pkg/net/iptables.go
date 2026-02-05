package net

import (
	"fmt"
	"os/exec"
	"strings"
)

type IPTablesRules struct {
	tapInterface string
	gatewayIP    string
	httpPort     int
	httpsPort    int
	rules        [][]string
}

func NewIPTablesRules(tapInterface, gatewayIP string, httpPort, httpsPort int) *IPTablesRules {
	return &IPTablesRules{
		tapInterface: tapInterface,
		gatewayIP:    gatewayIP,
		httpPort:     httpPort,
		httpsPort:    httpsPort,
	}
}

func (r *IPTablesRules) Setup() error {
	r.rules = [][]string{
		// Redirect HTTP (port 80) from guest to transparent proxy
		{"-t", "nat", "-A", "PREROUTING", "-i", r.tapInterface, "-p", "tcp", "--dport", "80",
			"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", r.gatewayIP, r.httpPort)},

		// Redirect HTTPS (port 443) from guest to transparent proxy
		{"-t", "nat", "-A", "PREROUTING", "-i", r.tapInterface, "-p", "tcp", "--dport", "443",
			"-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", r.gatewayIP, r.httpsPort)},

		// Allow forwarding for the TAP interface
		{"-I", "FORWARD", "1", "-i", r.tapInterface, "-j", "ACCEPT"},
		{"-I", "FORWARD", "2", "-o", r.tapInterface, "-j", "ACCEPT"},
	}

	for _, rule := range r.rules {
		if err := iptables(rule...); err != nil {
			r.Cleanup()
			return fmt.Errorf("failed to add iptables rule %v: %w", rule, err)
		}
	}

	return nil
}

func (r *IPTablesRules) Cleanup() error {
	var lastErr error
	for _, rule := range r.rules {
		deleteRule := make([]string, len(rule))
		copy(deleteRule, rule)
		
		// Find and replace -A or -I with -D
		for i, arg := range deleteRule {
			if arg == "-A" || arg == "-I" {
				deleteRule[i] = "-D"
				// For -I rules with position number, remove the position
				if arg == "-I" && i+2 < len(deleteRule) {
					// Check if position 2 (after chain name) is a number
					nextIdx := i + 2
					if _, err := fmt.Sscanf(deleteRule[nextIdx], "%d", new(int)); err == nil {
						// Remove the position number
						deleteRule = append(deleteRule[:nextIdx], deleteRule[nextIdx+1:]...)
					}
				}
				break
			}
		}
		if err := iptables(deleteRule...); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func iptables(args ...string) error {
	cmd := exec.Command("iptables", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %v failed: %s: %w", args, output, err)
	}
	return nil
}

func getDefaultInterface() string {
	cmd := exec.Command("ip", "route", "show", "default")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	// Parse "default via X.X.X.X dev INTERFACE ..."
	fields := strings.Fields(string(output))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func SetupNATMasquerade(guestSubnet string) error {
	// Auto-detect default interface
	defaultIface := getDefaultInterface()
	if defaultIface == "" {
		return fmt.Errorf("could not detect default network interface")
	}

	// Check if rule already exists
	checkCmd := exec.Command("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", guestSubnet, "-o", defaultIface, "-j", "MASQUERADE")
	if checkCmd.Run() == nil {
		// Rule already exists
		return nil
	}

	// Add MASQUERADE rule for guest traffic
	return iptables("-t", "nat", "-A", "POSTROUTING", "-s", guestSubnet, "-o", defaultIface, "-j", "MASQUERADE")
}
