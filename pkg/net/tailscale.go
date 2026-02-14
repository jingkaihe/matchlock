package net

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

type TailscaleDialerConfig struct {
	StateDir string
	Hostname string
	AuthKey  string
}

type tailscaleDialer struct {
	inner UpstreamDialer
}

type tailscaleStatus struct {
	BackendState string `json:"BackendState"`
	Self         struct {
		DNSName string `json:"DNSName"`
	} `json:"Self"`
	CurrentTailnet struct {
		MagicDNSSuffix string `json:"MagicDNSSuffix"`
	} `json:"CurrentTailnet"`
}

func NewTailscaleDialer(cfg *TailscaleDialerConfig) (UpstreamDialer, error) {
	if cfg == nil || cfg.AuthKey == "" {
		return nil, ErrTailscaleConfig
	}

	if err := ensureHostTailscale(cfg.AuthKey); err != nil {
		return nil, err
	}

	return &tailscaleDialer{inner: NewSystemDialer()}, nil
}

func ensureHostTailscale(authKey string) error {
	running, err := hostTailscaleRunning()
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	cmd := exec.Command("tailscale", "up", "--auth-key="+authKey)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return errx.With(ErrTailscaleUp, ": %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

func hostTailscaleRunning() (bool, error) {
	status, err := hostTailscaleStatus()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Non-zero status means tailscaled exists but is not up yet.
			return false, nil
		}
		return false, err
	}

	return strings.EqualFold(status.BackendState, "running"), nil
}

func HostTailscaleSearchDomains() ([]string, error) {
	status, err := hostTailscaleStatus()
	if err != nil {
		return nil, err
	}

	domains := extractTailscaleSearchDomains(status)
	if len(domains) == 0 {
		return nil, nil
	}
	return domains, nil
}

func hostTailscaleStatus() (*tailscaleStatus, error) {
	cmd := exec.Command("tailscale", "status", "--json")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, exitErr
		}
		return nil, errx.Wrap(ErrTailscaleCLI, err)
	}

	var status tailscaleStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return nil, errx.Wrap(ErrTailscaleCLI, err)
	}
	return &status, nil
}

func extractTailscaleSearchDomains(status *tailscaleStatus) []string {
	if status == nil {
		return nil
	}

	var domains []string
	add := func(d string) {
		d = normalizeDomain(d)
		if d == "" {
			return
		}
		for _, existing := range domains {
			if existing == d {
				return
			}
		}
		domains = append(domains, d)
	}

	add(status.CurrentTailnet.MagicDNSSuffix)
	add(tailnetDomainFromDNSName(status.Self.DNSName))
	return domains
}

func tailnetDomainFromDNSName(dnsName string) string {
	dnsName = normalizeDomain(dnsName)
	if dnsName == "" {
		return ""
	}
	_, domain, ok := strings.Cut(dnsName, ".")
	if !ok {
		return ""
	}
	return normalizeDomain(domain)
}

func normalizeDomain(domain string) string {
	domain = strings.TrimSpace(domain)
	domain = strings.TrimSuffix(domain, ".")
	return strings.ToLower(domain)
}

func (d *tailscaleDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d == nil || d.inner == nil {
		return nil, ErrTailscaleConfig
	}
	conn, err := d.inner.DialContext(ctx, network, address)
	if err != nil {
		return nil, errx.With(ErrTailscaleDial, " %s %s: %w", network, address, err)
	}
	return conn, nil
}

func (d *tailscaleDialer) Close() error {
	if d == nil || d.inner == nil {
		return nil
	}
	return d.inner.Close()
}
