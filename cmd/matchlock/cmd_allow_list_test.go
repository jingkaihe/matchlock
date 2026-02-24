package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAllowListHosts(t *testing.T) {
	hosts, err := parseAllowListHosts([]string{"api.openai.com,api.anthropic.com", "api.openai.com", " 8.8.8.8 "})
	require.NoError(t, err)
	assert.Equal(t, []string{"api.openai.com", "api.anthropic.com", "8.8.8.8"}, hosts)
}

func TestParseAllowListHostsRejectsEmpty(t *testing.T) {
	_, err := parseAllowListHosts([]string{",", "  "})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected one or more hosts")
}
