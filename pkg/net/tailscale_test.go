package net

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractTailscaleSearchDomains_PrefersMagicSuffix(t *testing.T) {
	status := &tailscaleStatus{}
	status.CurrentTailnet.MagicDNSSuffix = "taila540d.ts.net."
	status.Self.DNSName = "host.taila540d.ts.net."

	domains := extractTailscaleSearchDomains(status)
	assert.Equal(t, []string{"taila540d.ts.net"}, domains)
}

func TestExtractTailscaleSearchDomains_FallsBackToSelfDNSName(t *testing.T) {
	status := &tailscaleStatus{}
	status.Self.DNSName = "jhe-m3-air.taila540d.ts.net."

	domains := extractTailscaleSearchDomains(status)
	assert.Equal(t, []string{"taila540d.ts.net"}, domains)
}

func TestExtractTailscaleSearchDomains_NoDomain(t *testing.T) {
	status := &tailscaleStatus{}
	status.Self.DNSName = "localhost"

	domains := extractTailscaleSearchDomains(status)
	assert.Empty(t, domains)
}
