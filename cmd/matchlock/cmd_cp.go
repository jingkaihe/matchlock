package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/copyarchive"
	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
	"github.com/jingkaihe/matchlock/pkg/state"
)

var cpCmd = &cobra.Command{
	Use:   "cp [flags] SRC DEST",
	Short: "Copy files between the host and a running sandbox",
	Long: `Copy files or directories between the host and a running sandbox.

One of SRC and DEST is VM:PATH, where PATH is an absolute path in the sandbox.
The other is a host path, or "-" to read or write a tar archive on stdin or
stdout. Paths follow docker cp rules:

  SRC/.   copies the contents of the directory, not the directory itself
  DEST/   must be an existing directory
  DEST    an existing directory receives SRC inside it; otherwise SRC is
          copied to DEST, replacing an existing file

Files copied into the sandbox are owned by its user (image USER or --user)
unless --owner or --archive is given. When copying out, the sandbox is not
trusted: setuid and setgid bits are dropped, devices and symlinks that point
outside the destination are skipped, and --max-size and --max-files limit what
is written to the host.

The sandbox must have been started with --rm=false or --detach.`,
	Example: `  matchlock cp ./src/. vm-abc123:/workspace
  matchlock cp vm-abc123:/workspace/dist ./out
  git archive HEAD | matchlock cp - vm-abc123:/workspace
  matchlock cp vm-abc123:/var/log/app - > logs.tar`,
	Args: cobra.ExactArgs(2),
	RunE: runCp,
}

func init() {
	cpCmd.Flags().BoolP("archive", "a", false, "Keep the source's numeric UID/GID (copying in only)")
	cpCmd.Flags().String("owner", "", "Owner of copied files: uid, uid:gid, user, or user:group (copying in only)")
	cpCmd.Flags().BoolP("follow-link", "L", false, "Copy the target of SRC if it is a symlink")
	cpCmd.Flags().StringArray("exclude", nil, "Skip paths matching a .dockerignore-style pattern (repeatable)")
	cpCmd.Flags().Int64("max-size", copyarchive.DefaultMaxBytes>>20, "Maximum MB written to the host when copying out (0 for no limit)")
	cpCmd.Flags().Int64("max-files", copyarchive.DefaultMaxEntries, "Maximum entries written to the host when copying out (0 for no limit)")
	rootCmd.AddCommand(cpCmd)
}

func runCp(cmd *cobra.Command, args []string) error {
	archive, _ := cmd.Flags().GetBool("archive")
	owner, _ := cmd.Flags().GetString("owner")
	followLink, _ := cmd.Flags().GetBool("follow-link")
	exclude, _ := cmd.Flags().GetStringArray("exclude")
	maxSizeMB, _ := cmd.Flags().GetInt64("max-size")
	maxFiles, _ := cmd.Flags().GetInt64("max-files")

	src, dest := parseCopyArg(args[0]), parseCopyArg(args[1])
	err := validateCp(src, dest, cpFlags{
		owner:      owner,
		archive:    archive,
		followLink: followLink,
		exclude:    exclude,
		limitsSet:  cmd.Flags().Changed("max-size") || cmd.Flags().Changed("max-files"),
		maxSizeMB:  maxSizeMB,
		maxFiles:   maxFiles,
	})
	if err != nil {
		return err
	}
	toGuest := dest.vmID != ""
	remote := src
	if toGuest {
		remote = dest
	}

	socketPath, err := execSocketPath(remote.vmID)
	if err != nil {
		return err
	}
	guest := sandbox.RelayGuest{SocketPath: socketPath}
	ctx, cancel := contextWithSignal(context.Background())
	defer cancel()

	if toGuest {
		opts := copyarchive.ToGuestOptions{Owner: owner, PreserveOwner: archive, FollowLink: followLink, Exclude: exclude, OnSkip: warnCopySkip}
		if src.path == "-" {
			return copyarchive.TarToGuest(ctx, guest, os.Stdin, dest.path, opts)
		}
		return copyarchive.ToGuest(ctx, guest, src.path, dest.path, opts)
	}
	opts := copyarchive.FromGuestOptions{FollowLink: followLink, Exclude: exclude, MaxBytes: maxSizeMB << 20, MaxEntries: maxFiles, OnSkip: warnCopySkip}
	if dest.path == "-" {
		return copyarchive.TarFromGuest(ctx, guest, src.path, os.Stdout, opts)
	}
	return copyarchive.FromGuest(ctx, guest, src.path, dest.path, opts)
}

type cpFlags struct {
	owner      string
	archive    bool
	followLink bool
	exclude    []string
	// limitsSet reports that --max-size or --max-files was given explicitly.
	limitsSet bool
	maxSizeMB int64
	maxFiles  int64
}

// validateCp rejects invalid copies and flags that would otherwise be
// silently ignored.
func validateCp(src, dest copyArg, f cpFlags) error {
	toGuest := dest.vmID != ""
	switch {
	case src.vmID != "" && toGuest:
		return errx.With(ErrInvalidCopy, ": copying between sandboxes is not supported")
	case src.vmID == "" && !toGuest:
		return errx.With(ErrInvalidCopy, ": SRC or DEST must be VM:PATH")
	case !toGuest && (f.owner != "" || f.archive):
		return errx.With(ErrInvalidCopy, ": --owner and --archive only apply when copying into a sandbox")
	case f.owner != "" && f.archive:
		return errx.With(ErrInvalidCopy, ": --owner cannot be combined with --archive")
	case src.path == "-" && (len(f.exclude) > 0 || f.followLink):
		return errx.With(ErrInvalidCopy, ": --exclude and --follow-link do not apply to a tar archive read from stdin")
	case f.limitsSet && (toGuest || dest.path == "-"):
		return errx.With(ErrInvalidCopy, ": --max-size and --max-files only apply when copying out to a host path")
	case f.maxSizeMB < 0 || f.maxFiles < 0:
		return errx.With(ErrInvalidCopy, ": --max-size and --max-files must not be negative")
	}
	remote := src
	if toGuest {
		remote = dest
	}
	if !path.IsAbs(remote.path) {
		return errx.With(ErrInvalidCopy, ": sandbox path %q must be absolute", remote.path)
	}
	return nil
}

type copyArg struct {
	vmID string
	path string
}

// parseCopyArg splits VM:PATH. As with docker cp, an argument that is "-",
// absolute, or starts with "." is a host path, so a host path containing a
// colon can be written as ./name:with:colons.
func parseCopyArg(arg string) copyArg {
	if arg == "-" || filepath.IsAbs(arg) || strings.HasPrefix(arg, ".") {
		return copyArg{path: arg}
	}
	vmID, p, ok := strings.Cut(arg, ":")
	if !ok || vmID == "" {
		return copyArg{path: arg}
	}
	return copyArg{vmID: vmID, path: p}
}

// copySpec is a run --copy-in or --copy-out HOST:GUEST or GUEST:HOST pair.
type copySpec struct {
	src  string
	dest string
}

// parseCopySpecs parses run --copy-in (HOST_PATH:GUEST_PATH) or --copy-out
// (GUEST_PATH:HOST_PATH) values. Guest paths are absolute and cannot contain a
// colon, so the separator is the last colon for --copy-in and the first for
// --copy-out.
func parseCopySpecs(flag string, values []string, guestFirst bool) ([]copySpec, error) {
	specs := make([]copySpec, 0, len(values))
	for _, value := range values {
		i := strings.LastIndex(value, ":")
		if guestFirst {
			i = strings.Index(value, ":")
		}
		if i <= 0 || i == len(value)-1 {
			return nil, errx.With(ErrInvalidCopy, ": --%s %q: expected SRC:DEST", flag, value)
		}
		spec := copySpec{src: value[:i], dest: value[i+1:]}
		guestPath := spec.dest
		if guestFirst {
			guestPath = spec.src
		}
		if !path.IsAbs(guestPath) {
			return nil, errx.With(ErrInvalidCopy, ": --%s %q: sandbox path must be absolute", flag, value)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// execSocketPath returns the exec relay socket of a running VM.
func execSocketPath(vmID string) (string, error) {
	mgr := state.NewManager()
	vmState, err := mgr.Get(vmID)
	if err != nil {
		return "", errx.With(ErrVMNotFound, " %s: %w", vmID, err)
	}
	if vmState.Status != "running" {
		return "", fmt.Errorf("VM %s is not running (status: %s)", vmID, vmState.Status)
	}
	socketPath := mgr.ExecSocketPath(vmID)
	if _, err := os.Stat(socketPath); err != nil {
		return "", fmt.Errorf("exec socket not found for %s (was it started with --rm=false?)", vmID)
	}
	return socketPath, nil
}

// copyIntoSandbox runs run --copy-in before the command.
func copyIntoSandbox(ctx context.Context, g copyarchive.Guest, specs []copySpec) error {
	for _, spec := range specs {
		if err := copyarchive.ToGuest(ctx, g, spec.src, spec.dest, copyarchive.ToGuestOptions{OnSkip: warnCopySkip}); err != nil {
			return errx.With(ErrCopyIn, " %s: %w", spec.src, err)
		}
	}
	return nil
}

// copyOutOfSandbox runs run --copy-out after the command, attempting every
// copy even if one fails so that logs and partial results are still retrieved.
func copyOutOfSandbox(ctx context.Context, g copyarchive.Guest, specs []copySpec) error {
	var errs []error
	for _, spec := range specs {
		opts := copyarchive.FromGuestOptions{
			MaxBytes:   copyarchive.DefaultMaxBytes,
			MaxEntries: copyarchive.DefaultMaxEntries,
			OnSkip:     warnCopySkip,
		}
		if err := copyarchive.FromGuest(ctx, g, spec.src, spec.dest, opts); err != nil {
			errs = append(errs, errx.With(ErrCopyOut, " %s: %w", spec.src, err))
		}
	}
	return errors.Join(errs...)
}

// copyOutExit keeps a failed command's exit code, reporting any copy-out error
// on stderr, and otherwise returns the copy-out error.
func copyOutExit(copyErr error, exitCode int) error {
	if copyErr != nil && exitCode != 0 {
		fmt.Fprintln(os.Stderr, copyErr)
		return commandExit(exitCode)
	}
	if copyErr != nil {
		return copyErr
	}
	return commandExit(exitCode)
}

// warnCopySkip prints a skipped entry. Names and reasons can come from the
// sandbox, so control characters are not passed to the terminal.
func warnCopySkip(name, reason string) {
	fmt.Fprintf(os.Stderr, "Warning: skipped %q: %s\n", name, strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, reason))
}
