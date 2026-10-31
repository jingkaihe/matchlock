package sandbox

import (
	"context"
	"io"
	"net"

	"github.com/jingkaihe/matchlock/internal/copyarchive"
	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vsock"
)

// CopyIn extracts a copy archive into the guest. Unless req sets an owner or
// preserves the archive's ownership, entries are owned by the sandbox's
// configured user (image USER or --user), or root when none is set.
func (s *Sandbox) CopyIn(ctx context.Context, req copyarchive.CopyInRequest, archive io.Reader) error {
	if req.Owner == "" && !req.PreserveOwner && s.config.ImageCfg != nil {
		req.Owner = s.config.ImageCfg.User
	}
	conn, err := dialGuestExec(s.machine)
	if err != nil {
		return err
	}
	return vsock.CopyIn(ctx, conn, req, archive)
}

// CopyOut streams a copy archive of a guest path. The caller must close the
// returned reader.
func (s *Sandbox) CopyOut(ctx context.Context, req copyarchive.CopyOutRequest) (copyarchive.Source, io.ReadCloser, error) {
	conn, err := dialGuestExec(s.machine)
	if err != nil {
		return copyarchive.Source{}, nil, err
	}
	return vsock.CopyOut(ctx, conn, req)
}

func dialGuestExec(machine vm.Machine) (net.Conn, error) {
	dialer, ok := machine.(vm.VsockDialer)
	if !ok {
		return nil, ErrNoVsockDialer
	}
	conn, err := dialer.DialVsock(vsock.ServicePortExec)
	if err != nil {
		return nil, errx.Wrap(ErrCopyConnect, err)
	}
	return conn, nil
}
