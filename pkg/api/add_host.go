package api

import (
	"net"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

func ParseAddHosts(specs []string) ([]HostIPMapping, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	mappings := make([]HostIPMapping, 0, len(specs))
	for _, spec := range specs {
		mapping, err := ParseAddHost(spec)
		if err != nil {
			return nil, errx.With(err, " in %q", spec)
		}
		mappings = append(mappings, mapping)
	}

	return mappings, nil
}

func ParseAddHost(spec string) (HostIPMapping, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return HostIPMapping{}, errx.With(ErrAddHostSpecFormat, ": empty spec")
	}

	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return HostIPMapping{}, errx.With(ErrAddHostSpecFormat, ": %q (expected host:ip)", spec)
	}

	mapping := HostIPMapping{
		Host: strings.TrimSpace(parts[0]),
		IP:   strings.TrimSpace(parts[1]),
	}
	if err := ValidateAddHost(mapping); err != nil {
		return HostIPMapping{}, err
	}

	parsedIP := net.ParseIP(mapping.IP)
	mapping.IP = parsedIP.String()
	return mapping, nil
}

func ValidateAddHost(mapping HostIPMapping) error {
	if mapping.Host == "" {
		return errx.With(ErrAddHostHost, ": empty host")
	}
	if strings.ContainsAny(mapping.Host, " \t\n\r") {
		return errx.With(ErrAddHostHost, ": %q contains whitespace", mapping.Host)
	}
	if strings.Contains(mapping.Host, ":") {
		return errx.With(ErrAddHostHost, ": %q must not contain ':'", mapping.Host)
	}
	if mapping.IP == "" {
		return errx.With(ErrAddHostIP, ": empty ip")
	}
	if ip := net.ParseIP(mapping.IP); ip == nil {
		return errx.With(ErrAddHostIP, ": %q", mapping.IP)
	}
	return nil
}
