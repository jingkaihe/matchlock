package copyarchive

import (
	"context"
	"errors"
	"io"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// ToGuestOptions control a host-to-guest copy.
type ToGuestOptions struct {
	// Owner sets the owner of copied entries in the guest. Empty uses the
	// guest transport's default.
	Owner string
	// PreserveOwner keeps the source's numeric owner.
	PreserveOwner bool
	// FollowLink copies the target of a symlink source instead of the link.
	FollowLink bool
	// Exclude holds .dockerignore-style patterns relative to a directory
	// source.
	Exclude []string
	// OnSkip is called for host entries that cannot be copied.
	OnSkip func(name, reason string)
}

// FromGuestOptions control a guest-to-host copy.
type FromGuestOptions struct {
	// FollowLink copies the target of a symlink source instead of the link.
	FollowLink bool
	// Exclude holds .dockerignore-style patterns relative to a directory
	// source.
	Exclude []string
	// MaxBytes and MaxEntries limit what is written to the host; zero means no
	// limit.
	MaxBytes   int64
	MaxEntries int64
	// OnSkip is called for guest entries that are not written to the host.
	OnSkip func(name, reason string)
}

// ToGuest copies the host path src to dest in the guest.
func ToGuest(ctx context.Context, g Guest, src, dest string, opts ToGuestOptions) error {
	source, readPath, err := StatSource(src, opts.FollowLink)
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	writeErr := make(chan error, 1)
	go func() {
		err := WriteArchive(ctx, pw, readPath, source, WriteOptions{Exclude: opts.Exclude, OnSkip: opts.OnSkip})
		pw.CloseWithError(err)
		writeErr <- err
	}()

	copyErr := g.CopyIn(ctx, CopyInRequest{
		Dest:          dest,
		Source:        &source,
		Owner:         opts.Owner,
		PreserveOwner: opts.PreserveOwner,
	}, pr)
	// Unblock the archive writer if the guest stopped reading early.
	pr.Close()
	if err := <-writeErr; err != nil && !errors.Is(err, io.ErrClosedPipe) {
		// A host read failure is the root cause of the guest-side error.
		return err
	}
	return copyErr
}

// TarToGuest extracts the tar archive read from r into dest in the guest,
// which must be an existing directory.
func TarToGuest(ctx context.Context, g Guest, r io.Reader, dest string, opts ToGuestOptions) error {
	return g.CopyIn(ctx, CopyInRequest{
		Dest:          dest,
		Owner:         opts.Owner,
		PreserveOwner: opts.PreserveOwner,
	}, r)
}

// FromGuest copies src in the guest to the host path dest. The guest is not
// trusted: entries are written with HostExtractOptions.
func FromGuest(ctx context.Context, g Guest, src, dest string, opts FromGuestOptions) error {
	source, archive, err := g.CopyOut(ctx, CopyOutRequest{Path: src, FollowLink: opts.FollowLink, Exclude: opts.Exclude})
	if err != nil {
		return err
	}
	defer archive.Close()

	plan, err := PlanDestination(dest, &source)
	if err != nil {
		return err
	}
	if err := Extract(ctx, archive, plan, HostExtractOptions(opts.MaxBytes, opts.MaxEntries, opts.OnSkip)); err != nil {
		return err
	}
	// Read to the end so a guest-side failure after the archive is reported,
	// without letting the guest stream unbounded trailing data.
	n, err := io.Copy(io.Discard, io.LimitReader(archive, maxTrailingBytes+1))
	if err != nil {
		return err
	}
	if n > maxTrailingBytes {
		return errx.With(ErrArchive, ": unexpected data after the end of the archive")
	}
	return nil
}

// maxTrailingBytes bounds the padding read after a guest archive ends.
const maxTrailingBytes = 1 << 20

// TarFromGuest writes the copy archive of src in the guest to w.
func TarFromGuest(ctx context.Context, g Guest, src string, w io.Writer, opts FromGuestOptions) error {
	_, archive, err := g.CopyOut(ctx, CopyOutRequest{Path: src, FollowLink: opts.FollowLink, Exclude: opts.Exclude})
	if err != nil {
		return err
	}
	defer archive.Close()
	_, err = io.Copy(w, archive)
	return err
}
