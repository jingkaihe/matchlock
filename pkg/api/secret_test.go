package api

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSecretInlineValue(t *testing.T) {
	name, secret, err := ParseSecret("MY_KEY=sk-123@api.example.com")
	require.NoError(t, err)
	assert.Equal(t, "MY_KEY", name)
	assert.Equal(t, "sk-123", secret.Value)
	require.Len(t, secret.Hosts, 1)
	assert.Equal(t, "api.example.com", secret.Hosts[0])
}

func TestParseSecretMultipleHosts(t *testing.T) {
	name, secret, err := ParseSecret("TOKEN=abc@host1.com,host2.com")
	require.NoError(t, err)
	assert.Equal(t, "TOKEN", name)
	require.Len(t, secret.Hosts, 2)
	assert.Equal(t, "host1.com", secret.Hosts[0])
	assert.Equal(t, "host2.com", secret.Hosts[1])
}

func TestParseSecretFromEnv(t *testing.T) {
	os.Setenv("TEST_SECRET_ABC", "env-value")
	defer os.Unsetenv("TEST_SECRET_ABC")

	name, secret, err := ParseSecret("TEST_SECRET_ABC@api.test.com")
	require.NoError(t, err)
	assert.Equal(t, "TEST_SECRET_ABC", name)
	assert.Equal(t, "env-value", secret.Value)
}

func TestParseSecretFromEnvMissing(t *testing.T) {
	os.Unsetenv("NONEXISTENT_SECRET_XYZ")
	_, _, err := ParseSecret("NONEXISTENT_SECRET_XYZ@host.com")
	require.Error(t, err)
}

func TestParseSecretMissingAt(t *testing.T) {
	_, _, err := ParseSecret("MY_KEY=value")
	require.Error(t, err)
}

func TestParseSecretEmptyHosts(t *testing.T) {
	_, _, err := ParseSecret("MY_KEY=value@")
	require.Error(t, err)
}

func TestParseSecretEmptyName(t *testing.T) {
	_, _, err := ParseSecret("=value@host.com")
	require.Error(t, err)
}

func TestParseSecretValueWithEquals(t *testing.T) {
	name, secret, err := ParseSecret("KEY=val=ue@host.com")
	require.NoError(t, err)
	assert.Equal(t, "KEY", name)
	assert.Equal(t, "val=ue", secret.Value)
}

func TestParseSecretValueWithAtSign(t *testing.T) {
	name, secret, err := ParseSecret("KEY=user@pass@host.com")
	require.NoError(t, err)
	assert.Equal(t, "KEY", name)
	// LastIndex of @ means host is "host.com", value is "user@pass"
	assert.Equal(t, "user@pass", secret.Value)
	require.Len(t, secret.Hosts, 1)
	assert.Equal(t, "host.com", secret.Hosts[0])
}

func TestParseSecretHostTrimSpaces(t *testing.T) {
	_, secret, err := ParseSecret("K=v@ host1.com , host2.com ")
	require.NoError(t, err)
	assert.Equal(t, "host1.com", secret.Hosts[0])
	assert.Equal(t, "host2.com", secret.Hosts[1])
}
