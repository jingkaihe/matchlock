package main

import (
	"fmt"
	"net"
	"strings"
)

func normalizePortForwardAddresses(values []string) ([]string, error) {
	if len(values) == 0 {
		values = []string{"127.0.0.1"}
	}

	seen := make(map[string]struct{})
	result := make([]string, 0, len(values))

	for _, raw := range values {
		for _, item := range strings.Split(raw, ",") {
			addr := strings.TrimSpace(item)
			if addr == "" {
				continue
			}
			if addr != "localhost" && net.ParseIP(addr) == nil {
				return nil, fmt.Errorf("invalid bind address %q (use an IP literal or localhost)", addr)
			}
			if _, ok := seen[addr]; ok {
				continue
			}
			seen[addr] = struct{}{}
			result = append(result, addr)
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("at least one --address must be provided")
	}
	return result, nil
}
