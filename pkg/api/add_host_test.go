package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAddHost(t *testing.T) {
	mapping, err := ParseAddHost("example.internal:10.0.0.5")
	require.NoError(t, err)
	assert.Equal(t, "example.internal", mapping.Host)
	assert.Equal(t, "10.0.0.5", mapping.IP)
}

func TestParseAddHostIPv6(t *testing.T) {
	mapping, err := ParseAddHost("ip6.internal:2001:db8::10")
	require.NoError(t, err)
	assert.Equal(t, "ip6.internal", mapping.Host)
	assert.Equal(t, "2001:db8::10", mapping.IP)
}

func TestParseAddHosts(t *testing.T) {
	mappings, err := ParseAddHosts([]string{"api.internal:10.1.2.3", "db.internal:10.1.2.4"})
	require.NoError(t, err)
	require.Len(t, mappings, 2)
	assert.Equal(t, HostIPMapping{Host: "api.internal", IP: "10.1.2.3"}, mappings[0])
	assert.Equal(t, HostIPMapping{Host: "db.internal", IP: "10.1.2.4"}, mappings[1])
}

func TestParseAddHostsInvalidSpec(t *testing.T) {
	_, err := ParseAddHosts([]string{"missing-separator"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAddHostSpecFormat)
}

func TestValidateAddHostInvalidHost(t *testing.T) {
	err := ValidateAddHost(HostIPMapping{Host: "bad host", IP: "10.0.0.1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAddHostHost)
}

func TestValidateAddHostInvalidIP(t *testing.T) {
	err := ValidateAddHost(HostIPMapping{Host: "ok.internal", IP: "not-an-ip"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAddHostIP)
}
