package copyarchive

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// Owner is a numeric file owner.
type Owner struct {
	UID int
	GID int
}

// ExtractOptions control how archive entries are written.
type ExtractOptions struct {
	// Owner, when set, owns every created entry.
	Owner *Owner
	// PreserveOwner applies the numeric owner recorded in the archive.
	PreserveOwner bool
	// ExactModes applies archive permission bits exactly, including setuid,
	// setgid, and sticky bits. Otherwise special bits are dropped and the
	// process umask applies, as for an unprivileged tar.
	ExactModes bool
	// ConfineSymlinks treats the archive as untrusted: entries whose parent
	// path goes through a symlink are refused, and once extraction is done,
	// symlinks that resolve outside the destination are removed. A single
	// symlink copied as a file always points outside it, so it is skipped.
	ConfineSymlinks bool
	// MaxBytes limits the total size of regular files; zero means no limit.
	MaxBytes int64
	// MaxEntries limits the number of archive entries; zero means no limit.
	MaxEntries int64
	// OnSkip is called for entries that are not extracted.
	OnSkip func(name, reason string)
}

// HostExtractOptions returns the options for writing a guest-supplied archive
// on the host: special permission bits are dropped, the umask applies, nothing
// is chowned, and symlinks must stay inside the destination.
func HostExtractOptions(maxBytes, maxEntries int64, onSkip func(name, reason string)) ExtractOptions {
	return ExtractOptions{
		ConfineSymlinks: true,
		MaxBytes:        maxBytes,
		MaxEntries:      maxEntries,
		OnSkip:          onSkip,
	}
}

// Extract writes the copy archive read from r according to plan.
//
// All writes go through an os.Root for the destination directory, so archive
// names and symlinks cannot reach outside it. Existing entries are replaced
// rather than written through, so a symlink or hard link already at the
// destination is never followed. A new directory or file is assembled under a
// temporary name and renamed into place only when extraction succeeds.
func Extract(ctx context.Context, r io.Reader, plan Plan, opts ExtractOptions) (retErr error) {
	if plan.Source != nil {
		if err := ValidateSource(*plan.Source); err != nil {
			return err
		}
	}
	parent, err := os.OpenRoot(plan.Dir)
	if err != nil {
		return errx.Wrap(ErrDestination, err)
	}
	defer parent.Close()

	x := &extractor{plan: plan, opts: opts, parent: parent, files: map[string]bool{}}
	switch plan.Kind {
	case PlanIntoDir:
		x.root = parent
	case PlanNewDir, PlanFile:
		if !validName(plan.Name) {
			return errx.With(ErrDestination, ": invalid name %q", plan.Name)
		}
		x.temp, err = tempName(plan.Name)
		if err != nil {
			return err
		}
		if plan.Kind == PlanFile {
			x.root = parent
		}
		defer func() {
			if retErr != nil {
				_ = parent.RemoveAll(x.temp)
			}
		}()
	default:
		return errx.With(ErrDestination, ": unknown plan kind %d", plan.Kind)
	}
	defer x.closeRoot()

	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errx.Wrap(ErrArchive, err)
		}
		if err := x.entry(header, tr); err != nil {
			return err
		}
	}
	return x.finish()
}

type dirTime struct {
	name    string
	modTime time.Time
}

// createdLink is a symlink created from the archive entry name.
type createdLink struct {
	target string
	name   string
}

type extractor struct {
	plan   Plan
	opts   ExtractOptions
	parent *os.Root
	// root receives entries: parent, or the temporary directory for PlanNewDir.
	root *os.Root
	// temp is the temporary name inside parent for PlanNewDir and PlanFile.
	temp     string
	entries  int64
	bytes    int64
	files    map[string]bool
	links    []createdLink
	dirTimes []dirTime
	// seen and wrote track non-directory entries that were read and created.
	seen  bool
	wrote bool
}

func (x *extractor) closeRoot() {
	if x.root != nil && x.root != x.parent {
		_ = x.root.Close()
	}
}

func (x *extractor) entry(header *tar.Header, r io.Reader) error {
	if header.Typeflag == tar.TypeXGlobalHeader {
		return nil
	}
	x.entries++
	if x.opts.MaxEntries > 0 && x.entries > x.opts.MaxEntries {
		return errx.With(ErrLimit, ": more than %d entries", x.opts.MaxEntries)
	}
	name, err := entryName(header.Name)
	if err != nil {
		return err
	}
	target, isRoot, err := x.target(name)
	if err != nil {
		return err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		return x.dir(target, isRoot, header)
	case tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
		if isRoot {
			return errx.With(ErrArchive, ": %s: archive root must be a directory", header.Name)
		}
		if x.plan.Kind == PlanFile && x.seen {
			return errx.With(ErrArchive, ": %s: file archive has more than one entry", header.Name)
		}
		x.seen = true
		if err := x.ensureRoot(0o755); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeReg:
			return x.file(target, header, r)
		case tar.TypeSymlink:
			return x.symlink(target, name, header)
		default:
			return x.hardlink(target, name, header)
		}
	default:
		skip(x.opts.OnSkip, header.Name, "unsupported file type")
		return nil
	}
}

// target maps an archive name to a path inside x.root. isRoot reports the
// archive root entry, which carries a new directory's own metadata.
func (x *extractor) target(name string) (string, bool, error) {
	src := x.plan.Source
	if src == nil {
		return name, name == ".", nil
	}
	rel, ok := stripRoot(name, src.Name)
	if !ok || (!src.IsDir && rel != ".") {
		return "", false, errx.With(ErrArchive, ": entry %q is outside archive root %q", name, src.Name)
	}
	switch x.plan.Kind {
	case PlanFile:
		return x.temp, false, nil
	case PlanNewDir:
		return rel, rel == ".", nil
	default:
		if src.ContentsOnly {
			return rel, rel == ".", nil
		}
		return name, false, nil
	}
}

func (x *extractor) dir(target string, isRoot bool, header *tar.Header) error {
	if x.plan.Kind == PlanFile {
		return errx.With(ErrArchive, ": %s: file archive contains a directory", header.Name)
	}
	if isRoot {
		if x.plan.Kind != PlanNewDir {
			// Never alter an existing destination directory.
			return nil
		}
		if err := x.ensureRoot(x.dirPerm(header)); err != nil {
			return err
		}
		return x.applyMetadata(".", header, true)
	}
	if err := x.ensureRoot(0o755); err != nil {
		return err
	}
	if err := x.mkdirParents(target); err != nil {
		return err
	}
	info, err := x.root.Lstat(target)
	if err == nil && info.IsDir() {
		return nil
	}
	if err := x.replace(target, err); err != nil {
		return err
	}
	if err := x.root.Mkdir(target, x.dirPerm(header)); err != nil {
		return errx.Wrap(ErrDestination, err)
	}
	return x.applyMetadata(target, header, true)
}

func (x *extractor) file(target string, header *tar.Header, r io.Reader) error {
	x.bytes += header.Size
	if x.opts.MaxBytes > 0 && x.bytes > x.opts.MaxBytes {
		return errx.With(ErrLimit, ": more than %d bytes", x.opts.MaxBytes)
	}
	if err := x.prepare(target); err != nil {
		return err
	}
	f, err := x.root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, x.filePerm(header))
	if err != nil {
		return errx.Wrap(ErrDestination, err)
	}
	_, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		return errx.Wrap(ErrArchive, copyErr)
	}
	if closeErr != nil {
		return errx.Wrap(ErrDestination, closeErr)
	}
	x.files[target] = true
	x.wrote = true
	return x.applyMetadata(target, header, false)
}

func (x *extractor) symlink(target, name string, header *tar.Header) error {
	if x.opts.ConfineSymlinks && x.plan.Kind == PlanFile {
		skip(x.opts.OnSkip, name, fmt.Sprintf("symlink to %q points outside the destination", header.Linkname))
		return nil
	}
	if err := x.prepare(target); err != nil {
		return err
	}
	if err := x.root.Symlink(header.Linkname, target); err != nil {
		return errx.Wrap(ErrDestination, err)
	}
	x.links = append(x.links, createdLink{target: target, name: name})
	x.wrote = true
	return x.chown(target, header)
}

func (x *extractor) hardlink(target, name string, header *tar.Header) error {
	if x.plan.Kind == PlanFile {
		return errx.With(ErrArchive, ": %s: hard link cannot be copied as a file", name)
	}
	linkName, err := entryName(header.Linkname)
	if err != nil {
		return err
	}
	linkTarget, _, err := x.target(linkName)
	if err != nil {
		return err
	}
	// Only link to files written by this archive, never to existing files.
	if !x.files[linkTarget] {
		skip(x.opts.OnSkip, name, fmt.Sprintf("hard link to %q, which is not in the archive", header.Linkname))
		return nil
	}
	if err := x.prepare(target); err != nil {
		return err
	}
	if err := x.root.Link(linkTarget, target); err != nil {
		return errx.Wrap(ErrDestination, err)
	}
	x.files[target] = true
	x.wrote = true
	return nil
}

// ensureRoot creates the temporary directory for PlanNewDir on first use.
func (x *extractor) ensureRoot(perm fs.FileMode) error {
	if x.root != nil {
		return nil
	}
	if err := x.parent.Mkdir(x.temp, perm); err != nil {
		return errx.Wrap(ErrDestination, err)
	}
	root, err := x.parent.OpenRoot(x.temp)
	if err != nil {
		return errx.Wrap(ErrDestination, err)
	}
	x.root = root
	return nil
}

// prepare creates missing parents of target and removes whatever is at target.
func (x *extractor) prepare(target string) error {
	if err := x.mkdirParents(target); err != nil {
		return err
	}
	_, err := x.root.Lstat(target)
	return x.replace(target, err)
}

// replace removes an existing entry so it is replaced, not written through.
// lstatErr is the result of an Lstat of target.
func (x *extractor) replace(target string, lstatErr error) error {
	if errors.Is(lstatErr, os.ErrNotExist) {
		return nil
	}
	if lstatErr != nil {
		return errx.Wrap(ErrDestination, lstatErr)
	}
	return errx.Wrap(ErrDestination, x.root.RemoveAll(target))
}

func (x *extractor) mkdirParents(target string) error {
	dir := path.Dir(target)
	if dir == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(dir, "/") {
		current = path.Join(current, part)
		info, err := x.root.Lstat(current)
		if err == nil && info.Mode()&fs.ModeSymlink != 0 {
			if x.opts.ConfineSymlinks {
				return errx.With(ErrArchive, ": entry %q goes through symlink %q", target, current)
			}
			info, err = x.root.Stat(current)
		}
		if err == nil {
			if !info.IsDir() {
				return errx.With(ErrDestination, ": %s: not a directory", current)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return errx.Wrap(ErrDestination, err)
		}
		if err := x.root.Mkdir(current, 0o755); err != nil {
			return errx.Wrap(ErrDestination, err)
		}
		if x.opts.Owner != nil {
			if err := x.root.Lchown(current, x.opts.Owner.UID, x.opts.Owner.GID); err != nil {
				return errx.Wrap(ErrDestination, err)
			}
		}
	}
	return nil
}

func (x *extractor) applyMetadata(target string, header *tar.Header, isDir bool) error {
	if err := x.chown(target, header); err != nil {
		return err
	}
	// chown clears setuid and setgid bits, so set the mode afterwards.
	if x.opts.ExactModes {
		if err := x.root.Chmod(target, header.FileInfo().Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
			return errx.Wrap(ErrDestination, err)
		}
	}
	if isDir {
		// Adding entries changes a directory's mtime, so set it at the end.
		x.dirTimes = append(x.dirTimes, dirTime{name: target, modTime: header.ModTime})
		return nil
	}
	return errx.Wrap(ErrDestination, x.root.Chtimes(target, header.ModTime, header.ModTime))
}

func (x *extractor) chown(target string, header *tar.Header) error {
	uid, gid, ok := 0, 0, false
	switch {
	case x.opts.Owner != nil:
		uid, gid, ok = x.opts.Owner.UID, x.opts.Owner.GID, true
	case x.opts.PreserveOwner:
		uid, gid, ok = header.Uid, header.Gid, true
	}
	if !ok {
		return nil
	}
	return errx.Wrap(ErrDestination, x.root.Lchown(target, uid, gid))
}

func (x *extractor) finish() error {
	switch x.plan.Kind {
	case PlanNewDir:
		if err := x.ensureRoot(0o755); err != nil {
			return err
		}
	case PlanFile:
		if !x.wrote {
			if x.seen {
				// The only entry was skipped and already reported.
				return nil
			}
			return errx.With(ErrArchive, ": archive has no file")
		}
	}
	if x.opts.ConfineSymlinks {
		// Check links only now: a later entry can replace a directory with a
		// symlink, so a link that was safe when created may no longer be.
		if err := x.removeEscapingLinks(); err != nil {
			return err
		}
	}
	for i := len(x.dirTimes) - 1; i >= 0; i-- {
		dt := x.dirTimes[i]
		if err := x.root.Chtimes(dt.name, dt.modTime, dt.modTime); err != nil {
			return errx.Wrap(ErrDestination, err)
		}
	}
	if x.temp == "" {
		return nil
	}
	if x.plan.Kind == PlanNewDir {
		// The destination did not exist when planned; never replace one that
		// appeared since.
		if _, err := x.parent.Lstat(x.plan.Name); err == nil {
			return errx.With(ErrDestination, ": %s already exists", filepath.Join(x.plan.Dir, x.plan.Name))
		}
	} else if info, err := x.parent.Lstat(x.plan.Name); err == nil && info.IsDir() {
		return errx.With(ErrDestination, ": cannot replace directory %s with a file", filepath.Join(x.plan.Dir, x.plan.Name))
	}
	x.closeRoot()
	x.root = nil
	return errx.Wrap(ErrDestination, x.parent.Rename(x.temp, x.plan.Name))
}

// removeEscapingLinks removes symlinks created by the archive that resolve
// outside the destination.
func (x *extractor) removeEscapingLinks() error {
	for _, l := range x.links {
		info, err := x.root.Lstat(l.target)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return errx.Wrap(ErrDestination, err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		link, err := x.root.Readlink(l.target)
		if err != nil {
			return errx.Wrap(ErrDestination, err)
		}
		outside, err := resolvesOutside(x.root, path.Dir(l.target), link)
		if err != nil {
			return errx.Wrap(ErrDestination, err)
		}
		if !outside {
			continue
		}
		if err := x.root.Remove(l.target); err != nil {
			return errx.Wrap(ErrDestination, err)
		}
		skip(x.opts.OnSkip, l.name, fmt.Sprintf("symlink to %q points outside the destination", link))
	}
	return nil
}

// maxLinkHops bounds symlink resolution so that link loops end.
const maxLinkHops = 40

// resolvesOutside reports whether link, the target of a symlink in directory
// dir of root, resolves outside root. Symlinks met along the way are followed
// as the kernel would follow them. Components that do not exist are resolved
// lexically, and absolute targets are always outside.
func resolvesOutside(root *os.Root, dir, link string) (bool, error) {
	hops := 0
	var resolve func(base []string, target string) ([]string, bool, error)
	resolve = func(base []string, target string) ([]string, bool, error) {
		if path.IsAbs(target) {
			return nil, true, nil
		}
		cur := base
		for _, part := range strings.Split(target, "/") {
			switch part {
			case "", ".":
				continue
			case "..":
				if len(cur) == 0 {
					return nil, true, nil
				}
				cur = cur[:len(cur)-1]
				continue
			}
			next := append(cur[:len(cur):len(cur)], part)
			name := path.Join(next...)
			info, err := root.Lstat(name)
			switch {
			case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
				cur = next
				continue
			case err != nil:
				return nil, false, err
			case info.Mode()&fs.ModeSymlink == 0:
				cur = next
				continue
			}
			if hops++; hops > maxLinkHops {
				return nil, true, nil
			}
			target, err := root.Readlink(name)
			if err != nil {
				return nil, false, err
			}
			resolved, outside, err := resolve(cur, target)
			if outside || err != nil {
				return nil, outside, err
			}
			cur = resolved
		}
		return cur, false, nil
	}
	var base []string
	if dir != "." {
		base = strings.Split(dir, "/")
	}
	_, outside, err := resolve(base, link)
	return outside, err
}

func (x *extractor) dirPerm(header *tar.Header) fs.FileMode {
	perm := fs.FileMode(header.Mode) & fs.ModePerm
	if !x.opts.ExactModes {
		// Keep directories writable so their entries can be extracted.
		perm |= 0o700
	}
	return perm
}

func (x *extractor) filePerm(header *tar.Header) fs.FileMode {
	return fs.FileMode(header.Mode) & fs.ModePerm
}

// entryName cleans an archive name and rejects names that leave the archive.
func entryName(name string) (string, error) {
	if name == "" {
		return "", errx.With(ErrArchive, ": empty entry name")
	}
	if path.IsAbs(name) {
		return "", errx.With(ErrArchive, ": unsafe entry name %q", name)
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errx.With(ErrArchive, ": unsafe entry name %q", name)
	}
	return cleaned, nil
}

// stripRoot returns name relative to the archive root rootName.
func stripRoot(name, rootName string) (string, bool) {
	if rootName == "." {
		return name, true
	}
	if name == rootName {
		return ".", true
	}
	if rest, ok := strings.CutPrefix(name, rootName+"/"); ok {
		return rest, true
	}
	return "", false
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.Contains(name, "/")
}

func tempName(name string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errx.Wrap(ErrDestination, err)
	}
	return fmt.Sprintf(".%s.matchlock-cp-%s", name, hex.EncodeToString(b[:])), nil
}
