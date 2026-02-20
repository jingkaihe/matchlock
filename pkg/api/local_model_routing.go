package api

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseLocalModelRoutes builds a []LocalModelRoute from CLI flags.
//
// backendFlag is the --local-model-backend value ("HOST:PORT" or empty).
// routeFlags is the --local-model-route values, each in format:
//
//	"SOURCE_HOST/SOURCE_MODEL=TARGET_MODEL[@HOST:PORT]"
//
// Routes are grouped by source host. If routeFlags is empty, returns nil.
func ParseLocalModelRoutes(backendFlag string, routeFlags []string) ([]LocalModelRoute, error) {
	if len(routeFlags) == 0 {
		return nil, nil
	}

	// Parse default backend
	defaultHost := "127.0.0.1"
	defaultPort := 11434
	if backendFlag != "" {
		h, p, err := parseHostPort(backendFlag)
		if err != nil {
			return nil, fmt.Errorf("invalid --local-model-backend %q: %w", backendFlag, err)
		}
		defaultHost = h
		defaultPort = p
	}

	// Group routes by source host
	routeMap := make(map[string]*LocalModelRoute) // keyed by source host
	routeOrder := make([]string, 0)               // preserve insertion order

	for _, flag := range routeFlags {
		sourceHost, sourceModel, targetModel, overrideHost, overridePort, err := parseRouteFlag(flag)
		if err != nil {
			return nil, err
		}

		route, exists := routeMap[sourceHost]
		if !exists {
			route = &LocalModelRoute{
				SourceHost:  sourceHost,
				BackendHost: defaultHost,
				BackendPort: defaultPort,
				Models:      make(map[string]ModelRoute),
			}
			routeMap[sourceHost] = route
			routeOrder = append(routeOrder, sourceHost)
		}

		modelRoute := ModelRoute{Target: targetModel}
		if overrideHost != "" || overridePort > 0 {
			modelRoute.BackendHost = overrideHost
			modelRoute.BackendPort = overridePort
		}
		route.Models[sourceModel] = modelRoute
	}

	// Build result in insertion order
	result := make([]LocalModelRoute, 0, len(routeOrder))
	for _, host := range routeOrder {
		result = append(result, *routeMap[host])
	}
	return result, nil
}

// parseRouteFlag parses "SOURCE_HOST/SOURCE_MODEL=TARGET[@HOST:PORT]"
func parseRouteFlag(flag string) (sourceHost, sourceModel, target, overrideHost string, overridePort int, err error) {
	// Split off optional @HOST:PORT from the end
	atIdx := strings.LastIndex(flag, "@")
	mainPart := flag
	if atIdx != -1 {
		backendPart := flag[atIdx+1:]
		mainPart = flag[:atIdx]
		overrideHost, overridePort, err = parseHostPort(backendPart)
		if err != nil {
			return "", "", "", "", 0, fmt.Errorf("invalid backend override in %q: %w", flag, ErrLocalModelRouteFormat)
		}
	}

	// Split SOURCE_HOST/SOURCE_MODEL=TARGET
	eqIdx := strings.LastIndex(mainPart, "=")
	if eqIdx == -1 {
		return "", "", "", "", 0, fmt.Errorf("missing '=' in %q: expected SOURCE_HOST/MODEL=TARGET[@HOST:PORT]: %w", flag, ErrLocalModelRouteFormat)
	}
	sourceSpec := mainPart[:eqIdx]
	target = mainPart[eqIdx+1:]
	if target == "" {
		return "", "", "", "", 0, fmt.Errorf("empty target model in %q: %w", flag, ErrLocalModelRouteFormat)
	}

	// Split SOURCE_HOST from SOURCE_MODEL at the first "/"
	slashIdx := strings.Index(sourceSpec, "/")
	if slashIdx == -1 {
		return "", "", "", "", 0, fmt.Errorf("missing '/' in %q: expected SOURCE_HOST/MODEL=TARGET: %w", flag, ErrLocalModelRouteFormat)
	}
	sourceHost = sourceSpec[:slashIdx]
	sourceModel = sourceSpec[slashIdx+1:]

	if sourceHost == "" {
		return "", "", "", "", 0, fmt.Errorf("empty source host in %q: %w", flag, ErrLocalModelRouteFormat)
	}
	if sourceModel == "" {
		return "", "", "", "", 0, fmt.Errorf("empty source model in %q: %w", flag, ErrLocalModelRouteFormat)
	}

	return sourceHost, sourceModel, target, overrideHost, overridePort, nil
}

// parseHostPort splits "HOST:PORT" into host string and port int.
func parseHostPort(s string) (string, int, error) {
	// Handle IPv6 addresses like [::1]:port
	if strings.HasPrefix(s, "[") {
		closeBracket := strings.Index(s, "]")
		if closeBracket == -1 {
			return "", 0, fmt.Errorf("unclosed bracket in %q", s)
		}
		host := s[1:closeBracket]
		rest := s[closeBracket+1:]
		if rest == "" || rest[0] != ':' {
			return "", 0, fmt.Errorf("expected :PORT after ] in %q", s)
		}
		port, err := strconv.Atoi(rest[1:])
		if err != nil || port <= 0 || port > 65535 {
			return "", 0, fmt.Errorf("invalid port in %q", s)
		}
		return host, port, nil
	}

	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("expected HOST:PORT, got %q", s)
	}
	host := parts[0]
	port, err := strconv.Atoi(parts[1])
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port in %q", s)
	}
	return host, port, nil
}
