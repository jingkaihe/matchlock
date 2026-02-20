package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLocalModelRoutes_Empty(t *testing.T) {
	routes, err := ParseLocalModelRoutes("", nil)
	require.NoError(t, err)
	assert.Nil(t, routes)
}

func TestParseLocalModelRoutes_SingleRoute(t *testing.T) {
	routes, err := ParseLocalModelRoutes("127.0.0.1:11434", []string{
		"openrouter.ai/meta-llama/llama-3.1-8b-instruct=llama3.1:8b",
	})
	require.NoError(t, err)
	require.Len(t, routes, 1)

	assert.Equal(t, "openrouter.ai", routes[0].SourceHost)
	assert.Equal(t, "127.0.0.1", routes[0].BackendHost)
	assert.Equal(t, 11434, routes[0].BackendPort)
	require.Contains(t, routes[0].Models, "meta-llama/llama-3.1-8b-instruct")
	assert.Equal(t, "llama3.1:8b", routes[0].Models["meta-llama/llama-3.1-8b-instruct"].Target)
}

func TestParseLocalModelRoutes_MultipleModelsSameHost(t *testing.T) {
	routes, err := ParseLocalModelRoutes("127.0.0.1:11434", []string{
		"openrouter.ai/meta-llama/llama-3.1-8b-instruct=llama3.1:8b",
		"openrouter.ai/qwen/qwen-2.5-coder-32b-instruct=qwen2.5-coder:32b",
	})
	require.NoError(t, err)
	require.Len(t, routes, 1, "Both models share the same source host")

	assert.Len(t, routes[0].Models, 2)
	assert.Equal(t, "llama3.1:8b", routes[0].Models["meta-llama/llama-3.1-8b-instruct"].Target)
	assert.Equal(t, "qwen2.5-coder:32b", routes[0].Models["qwen/qwen-2.5-coder-32b-instruct"].Target)
}

func TestParseLocalModelRoutes_MultipleSources(t *testing.T) {
	routes, err := ParseLocalModelRoutes("127.0.0.1:11434", []string{
		"openrouter.ai/meta-llama/llama-3.1-8b-instruct=llama3.1:8b",
		"api.openai.com/gpt-4o-mini=llama3.1:8b",
	})
	require.NoError(t, err)
	require.Len(t, routes, 2)

	assert.Equal(t, "openrouter.ai", routes[0].SourceHost)
	assert.Equal(t, "api.openai.com", routes[1].SourceHost)
}

func TestParseLocalModelRoutes_PerModelBackendOverride(t *testing.T) {
	routes, err := ParseLocalModelRoutes("127.0.0.1:11434", []string{
		"openrouter.ai/meta-llama/llama-3.1-8b-instruct=llama3.1:8b",
		"openrouter.ai/qwen/qwen-2.5-coder-32b-instruct=qwen2.5-coder:32b@10.0.0.5:8080",
	})
	require.NoError(t, err)
	require.Len(t, routes, 1)

	// Default model: no override
	m1 := routes[0].Models["meta-llama/llama-3.1-8b-instruct"]
	assert.Empty(t, m1.BackendHost)
	assert.Zero(t, m1.BackendPort)

	// Override model
	m2 := routes[0].Models["qwen/qwen-2.5-coder-32b-instruct"]
	assert.Equal(t, "10.0.0.5", m2.BackendHost)
	assert.Equal(t, 8080, m2.BackendPort)
}

func TestParseLocalModelRoutes_DefaultBackend(t *testing.T) {
	// No --local-model-backend flag, should use defaults
	routes, err := ParseLocalModelRoutes("", []string{
		"openrouter.ai/meta-llama/llama-3.1-8b-instruct=llama3.1:8b",
	})
	require.NoError(t, err)
	require.Len(t, routes, 1)

	assert.Equal(t, "127.0.0.1", routes[0].BackendHost)
	assert.Equal(t, 11434, routes[0].BackendPort)
}

func TestParseLocalModelRoutes_InvalidFormat_NoEquals(t *testing.T) {
	_, err := ParseLocalModelRoutes("", []string{
		"openrouter.ai/meta-llama/llama-3.1-8b-instruct",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLocalModelRouteFormat)
}

func TestParseLocalModelRoutes_InvalidFormat_NoSlash(t *testing.T) {
	_, err := ParseLocalModelRoutes("", []string{
		"meta-llama=llama3.1:8b",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLocalModelRouteFormat)
}

func TestParseLocalModelRoutes_InvalidFormat_EmptyTarget(t *testing.T) {
	_, err := ParseLocalModelRoutes("", []string{
		"openrouter.ai/meta-llama/llama-3.1-8b-instruct=",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLocalModelRouteFormat)
}

func TestParseLocalModelRoutes_InvalidBackendFlag(t *testing.T) {
	_, err := ParseLocalModelRoutes("not-a-host-port", []string{
		"openrouter.ai/model=target",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--local-model-backend")
}

func TestLocalModelRoute_GetPath_Default(t *testing.T) {
	r := &LocalModelRoute{}
	assert.Equal(t, "/api/v1/chat/completions", r.GetPath())
}

func TestLocalModelRoute_GetPath_Custom(t *testing.T) {
	r := &LocalModelRoute{Path: "/v1/chat/completions"}
	assert.Equal(t, "/v1/chat/completions", r.GetPath())
}

func TestLocalModelRoute_GetBackendHost_Default(t *testing.T) {
	r := &LocalModelRoute{}
	assert.Equal(t, "127.0.0.1", r.GetBackendHost())
}

func TestLocalModelRoute_GetBackendPort_Default(t *testing.T) {
	r := &LocalModelRoute{}
	assert.Equal(t, 11434, r.GetBackendPort())
}

func TestModelRoute_EffectiveBackend_FallbackToRoute(t *testing.T) {
	m := &ModelRoute{Target: "llama3.1:8b"}
	assert.Equal(t, "192.168.1.1", m.EffectiveBackendHost("192.168.1.1"))
	assert.Equal(t, 8080, m.EffectiveBackendPort(8080))
}

func TestModelRoute_EffectiveBackend_Override(t *testing.T) {
	m := &ModelRoute{Target: "llama3.1:8b", BackendHost: "10.0.0.5", BackendPort: 9090}
	assert.Equal(t, "10.0.0.5", m.EffectiveBackendHost("192.168.1.1"))
	assert.Equal(t, 9090, m.EffectiveBackendPort(8080))
}
