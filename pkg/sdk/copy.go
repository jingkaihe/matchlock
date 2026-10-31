package sdk

import (
	"context"
	"os"
	"path/filepath"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// CopyInOptions control CopyIn.
type CopyInOptions struct {
	// Owner of the copied files in the sandbox: "uid", "uid:gid", "user", or
	// "user:group". Empty uses the sandbox's user (image USER or WithUser),
	// or root when none is set.
	Owner string
	// Archive keeps the source's numeric UID/GID instead. It cannot be
	// combined with Owner.
	Archive bool
	// FollowLink copies the target of src if it is a symlink.
	FollowLink bool
	// Exclude holds .dockerignore-style patterns relative to a directory src.
	Exclude []string
}

// CopyOutOptions control CopyOut.
type CopyOutOptions struct {
	// FollowLink copies the target of src if it is a symlink.
	FollowLink bool
	// Exclude holds .dockerignore-style patterns relative to a directory src.
	Exclude []string
	// MaxSize limits the bytes written to the host. Zero uses the default
	// (10 GiB) and a negative value disables the limit.
	MaxSize int64
	// MaxFiles limits the entries written to the host. Zero uses the default
	// (1,000,000) and a negative value disables the limit.
	MaxFiles int64
}

// CopyIn copies the host file or directory src to the absolute sandbox path
// dest, following docker cp rules: a src ending in "/." copies a directory's
// contents, and a dest ending in "/" must be an existing directory.
func (c *Client) CopyIn(ctx context.Context, src, dest string, opts *CopyInOptions) error {
	if opts == nil {
		opts = &CopyInOptions{}
	}
	if opts.Owner != "" && opts.Archive {
		return errx.With(ErrInvalidCopy, ": Owner cannot be combined with Archive")
	}
	hostPath, err := absHostPath(src)
	if err != nil {
		return err
	}
	params := map[string]interface{}{"src": hostPath, "dest": dest}
	if opts.Owner != "" {
		params["owner"] = opts.Owner
	}
	if opts.Archive {
		params["archive"] = true
	}
	addCopyParams(params, opts.FollowLink, opts.Exclude)
	_, err = c.sendRequestCtx(ctx, "copy_in", params, nil)
	return err
}

// CopyOut copies the absolute sandbox path src to the host path dest, with the
// same path rules as CopyIn. The sandbox is not trusted: setuid and setgid bits
// are dropped, devices and symlinks that point outside dest are skipped, and
// MaxSize and MaxFiles limit what is written.
func (c *Client) CopyOut(ctx context.Context, src, dest string, opts *CopyOutOptions) error {
	if opts == nil {
		opts = &CopyOutOptions{}
	}
	if c.useSudo {
		return ErrCopyOutWithSudo
	}
	hostPath, err := absHostPath(dest)
	if err != nil {
		return err
	}
	params := map[string]interface{}{"src": src, "dest": hostPath}
	addCopyParams(params, opts.FollowLink, opts.Exclude)
	for key, limit := range map[string]int64{"max_size": opts.MaxSize, "max_files": opts.MaxFiles} {
		switch {
		case limit > 0:
			params[key] = limit
		case limit < 0:
			params[key] = 0
		}
	}
	_, err = c.sendRequestCtx(ctx, "copy_out", params, nil)
	return err
}

func addCopyParams(params map[string]interface{}, followLink bool, exclude []string) {
	if followLink {
		params["follow_link"] = true
	}
	if len(exclude) > 0 {
		params["exclude"] = exclude
	}
}

// absHostPath makes p absolute for the matchlock rpc process without cleaning
// it, so the "/." and trailing "/" suffixes keep their meaning.
func absHostPath(p string) (string, error) {
	if p == "" {
		return "", errx.With(ErrInvalidCopy, ": host path is empty")
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", errx.Wrap(ErrInvalidCopy, err)
	}
	return wd + string(filepath.Separator) + p, nil
}
