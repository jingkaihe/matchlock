package api

import (
	"strconv"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// PortForward maps a local host port to a remote guest port.
type PortForward struct {
	LocalPort  int `json:"local_port"`
	RemotePort int `json:"remote_port"`
}

// PortForwardBinding is a realized local listener binding.
type PortForwardBinding struct {
	Address    string `json:"address"`
	LocalPort  int    `json:"local_port"`
	RemotePort int    `json:"remote_port"`
}

// ParsePortForwards parses specs in the form [LOCAL_PORT:]REMOTE_PORT.
func ParsePortForwards(specs []string) ([]PortForward, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	result := make([]PortForward, 0, len(specs))
	for _, spec := range specs {
		pf, err := ParsePortForward(spec)
		if err != nil {
			return nil, err
		}
		result = append(result, pf)
	}
	return result, nil
}

// ParsePortForward parses a single spec in the form [LOCAL_PORT:]REMOTE_PORT.
func ParsePortForward(spec string) (PortForward, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return PortForward{}, errx.With(ErrPortForwardSpecFormat, ": empty spec")
	}

	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		remotePort, err := parsePort(parts[0], "remote")
		if err != nil {
			return PortForward{}, err
		}
		return PortForward{
			LocalPort:  remotePort,
			RemotePort: remotePort,
		}, nil
	case 2:
		localPort, err := parsePort(parts[0], "local")
		if err != nil {
			return PortForward{}, err
		}
		remotePort, err := parsePort(parts[1], "remote")
		if err != nil {
			return PortForward{}, err
		}
		return PortForward{
			LocalPort:  localPort,
			RemotePort: remotePort,
		}, nil
	default:
		return PortForward{}, errx.With(ErrPortForwardSpecFormat, ": %q (expected [LOCAL_PORT:]REMOTE_PORT)", spec)
	}
}

func parsePort(value string, role string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errx.With(ErrPortForwardSpecFormat, ": empty %s port", role)
	}

	p, err := strconv.Atoi(value)
	if err != nil {
		return 0, errx.With(ErrPortForwardPort, " %q: %v", value, err)
	}
	if p < 1 || p > 65535 {
		return 0, errx.With(ErrPortForwardPort, " %d: must be in range 1-65535", p)
	}
	return p, nil
}
