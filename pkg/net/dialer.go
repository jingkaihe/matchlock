package net

import (
	"context"
	"net"
	"time"
)

// UpstreamDialer abstracts outbound network dials used by interception paths.
type UpstreamDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
	Close() error
}

type systemDialer struct {
	dialer net.Dialer
}

func NewSystemDialer() UpstreamDialer {
	return &systemDialer{
		dialer: net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		},
	}
}

func (d *systemDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dialer.DialContext(ctx, network, address)
}

func (d *systemDialer) Close() error {
	return nil
}
