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
	cmd := exec.Command("tailscale", "status", "--json")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Non-zero status means tailscaled exists but is not up yet.
			return false, nil
		}
		return false, errx.Wrap(ErrTailscaleCLI, err)
	}

	var status struct {
		BackendState string `json:"BackendState"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return false, errx.Wrap(ErrTailscaleCLI, err)
	}
	return strings.EqualFold(status.BackendState, "running"), nil
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
