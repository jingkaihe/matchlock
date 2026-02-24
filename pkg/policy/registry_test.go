package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLookupFactory_BuiltIn(t *testing.T) {
	for _, name := range []string{"host_filter", "secret_injector", "local_model_router", "usage_logger"} {
		f, ok := LookupFactory(name)
		assert.True(t, ok, "built-in %q should be registered", name)
		assert.NotNil(t, f)
	}
}

func TestLookupFactory_Unknown(t *testing.T) {
	_, ok := LookupFactory("nonexistent")
	assert.False(t, ok)
}

func TestRegisteredTypes(t *testing.T) {
	types := RegisteredTypes()
	assert.Contains(t, types, "host_filter")
	assert.Contains(t, types, "secret_injector")
	assert.Contains(t, types, "local_model_router")
	assert.Contains(t, types, "usage_logger")
}
