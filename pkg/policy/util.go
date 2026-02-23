package policy

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"strings"
)

// generatePlaceholder creates a random SANDBOX_SECRET_ placeholder string.
func generatePlaceholder() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "SANDBOX_SECRET_" + hex.EncodeToString(b)
}

func matchGlob(pattern, str string) bool {
	if pattern == "*" {
		return true
	}

	// Simple prefix wildcard: *.example.com
	if strings.HasPrefix(pattern, "*.") && !strings.Contains(pattern[2:], "*") {
		suffix := pattern[1:]
		return strings.HasSuffix(str, suffix)
	}

	// Simple suffix wildcard: example.*
	if strings.HasSuffix(pattern, ".*") && !strings.Contains(pattern[:len(pattern)-2], "*") {
		prefix := pattern[:len(pattern)-2]
		return strings.HasPrefix(str, prefix+".")
	}

	// General glob matching with * as wildcard
	if strings.Contains(pattern, "*") {
		return matchWildcard(pattern, str)
	}

	return pattern == str
}

// matchWildcard handles patterns with * wildcards anywhere
func matchWildcard(pattern, str string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == str
	}

	// Check prefix (before first *)
	if parts[0] != "" && !strings.HasPrefix(str, parts[0]) {
		return false
	}
	str = str[len(parts[0]):]

	// Check suffix (after last *)
	lastPart := parts[len(parts)-1]
	if lastPart != "" && !strings.HasSuffix(str, lastPart) {
		return false
	}
	if lastPart != "" {
		str = str[:len(str)-len(lastPart)]
	}

	// Check middle parts in order
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "" {
			continue
		}
		idx := strings.Index(str, parts[i])
		if idx < 0 {
			return false
		}
		str = str[idx+len(parts[i]):]
	}

	return true
}

func isPrivateIP(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return false
		}
		ip = ips[0]
	}

	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}

	for _, cidr := range privateRanges {
		_, network, _ := net.ParseCIDR(cidr)
		if network.Contains(ip) {
			return true
		}
	}

	return false
}
