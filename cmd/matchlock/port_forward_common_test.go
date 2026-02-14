package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePortForwardAddressesDefaults(t *testing.T) {
	addrs, err := normalizePortForwardAddresses(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.1"}, addrs)
}

func TestNormalizePortForwardAddressesDedupes(t *testing.T) {
	addrs, err := normalizePortForwardAddresses([]string{"127.0.0.1,localhost", "127.0.0.1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.1", "localhost"}, addrs)
}

func TestNormalizePortForwardAddressesRejectsInvalid(t *testing.T) {
	_, err := normalizePortForwardAddresses([]string{"not-an-ip"})
	require.Error(t, err)
}
