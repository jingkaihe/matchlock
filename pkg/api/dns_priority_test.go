package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetworkConfigGetDNSPriorityDefault(t *testing.T) {
	var nilConfig *NetworkConfig
	assert.Equal(t, DNSPriorityIPv4, nilConfig.GetDNSPriority(), "a nil config must default to ipv4")
	assert.Equal(t, DNSPriorityIPv4, (&NetworkConfig{}).GetDNSPriority(), "an unset priority must default to ipv4")
	assert.Equal(t, DNSPriorityIPv4, (&NetworkConfig{DNSPriority: DNSPriorityIPv4}).GetDNSPriority())
	assert.Equal(t, DNSPriorityIPv6, (&NetworkConfig{DNSPriority: DNSPriorityIPv6}).GetDNSPriority())
}

func TestNetworkConfigValidateDNSPriority(t *testing.T) {
	require.NoError(t, (&NetworkConfig{}).Validate(), "unset is valid (it defaults to ipv4)")
	require.NoError(t, (&NetworkConfig{DNSPriority: DNSPriorityIPv4}).Validate())
	require.NoError(t, (&NetworkConfig{DNSPriority: DNSPriorityIPv6}).Validate())

	err := (&NetworkConfig{DNSPriority: "IPv6"}).Validate()
	require.ErrorIs(t, err, ErrInvalidConfig, "an unsupported priority must be rejected")
	assert.Contains(t, err.Error(), "dns_priority", "the error must name the offending field")
	assert.Contains(t, err.Error(), "IPv6", "the error must quote the offending value")

	require.ErrorIs(t, (&NetworkConfig{DNSPriority: "v4"}).Validate(), ErrInvalidConfig)
	require.ErrorIs(t, (&NetworkConfig{DNSPriority: "ipv4 "}).Validate(), ErrInvalidConfig,
		"values are exact: no trimming or aliasing")
}

func TestNetworkConfigDNSPriorityJSON(t *testing.T) {
	raw, err := json.Marshal(&NetworkConfig{DNSPriority: DNSPriorityIPv6})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"dns_priority":"ipv6"`)

	raw, err = json.Marshal(&NetworkConfig{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "dns_priority", "an unset priority must be omitted from JSON")

	var decoded NetworkConfig
	require.NoError(t, json.Unmarshal([]byte(`{"dns_priority":"ipv4"}`), &decoded))
	assert.Equal(t, DNSPriorityIPv4, decoded.DNSPriority)
	assert.Equal(t, DNSPriorityIPv4, decoded.GetDNSPriority())
}
