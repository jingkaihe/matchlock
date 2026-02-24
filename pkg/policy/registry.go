package policy

import (
	"encoding/json"
	"log/slog"
	"sync"
)

// PluginFactory creates a plugin from its JSON config blob.
// The logger is pre-scoped with component=policy and plugin=<name>
// by the engine before calling the factory. Plugins should store
// it directly and use it for Debug-level logging.
type PluginFactory func(config json.RawMessage, logger *slog.Logger) (Plugin, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]PluginFactory{}
)

func init() {
	// Register built-in plugin factories.
	Register("host_filter", NewHostFilterPluginFromConfig)
	Register("secret_injector", NewSecretInjectorPluginFromConfig)
	Register("local_model_router", NewLocalModelRouterPluginFromConfig)
	Register("usage_logger", NewUsageLoggerPluginFromConfig)
}

// Register adds a plugin factory to the global registry.
// Third-party compiled-in plugins call this in their own init() functions.
// Panics if a factory is already registered for the given type name.
func Register(typeName string, factory PluginFactory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[typeName]; exists {
		panic("policy: duplicate plugin registration for type " + typeName)
	}
	registry[typeName] = factory
}

// LookupFactory returns the factory for a plugin type name, if registered.
func LookupFactory(typeName string) (PluginFactory, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[typeName]
	return f, ok
}

// RegisteredTypes returns the names of all registered plugin types.
// Useful for validation and documentation.
func RegisteredTypes() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	types := make([]string, 0, len(registry))
	for name := range registry {
		types = append(types, name)
	}
	return types
}
