package copyarchive

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/moby/patternmatcher"

	"github.com/jingkaihe/matchlock/internal/errx"
)

const archiveBufferSize = 256 * 1024

// WriteOptions control how a copy archive is produced.
type WriteOptions struct {
	// Exclude holds .dockerignore-style patterns, relative to a directory
	// source, for entries to leave out.
	Exclude []string
	// OnSkip is called for entries that cannot be archived.
	OnSkip func(name, reason string)
}

// StatSource resolves p as a copy source using docker cp rules: a trailing
// "/." copies a directory's contents, a trailing "/" requires a directory, and
// a symlink is copied as a link unless followLink is set. It returns the source
// description and the path to read from.
func StatSource(p string, followLink bool) (Source, string, error) {
	if p == "" {
		return Source{}, "", errx.With(ErrSource, ": path is empty")
	}
	contentsOnly := p == "." || strings.HasSuffix(p, "/.")
	requireDir := contentsOnly || strings.HasSuffix(p, "/")

	abs, err := filepath.Abs(p)
	if err != nil {
		return Source{}, "", errx.Wrap(ErrSource, err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return Source{}, "", errx.Wrap(ErrSource, err)
	}
	readPath := abs
	if info.Mode()&os.ModeSymlink != 0 && (followLink || requireDir) {
		readPath, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return Source{}, "", errx.Wrap(ErrSource, err)
		}
		info, err = os.Stat(readPath)
		if err != nil {
			return Source{}, "", errx.Wrap(ErrSource, err)
		}
	}
	if requireDir && !info.IsDir() {
		return Source{}, "", errx.With(ErrSource, ": %s: not a directory", p)
	}

	name := filepath.Base(abs)
	if name == string(filepath.Separator) {
		name = "."
		contentsOnly = true
	}
	return Source{Name: name, IsDir: info.IsDir(), ContentsOnly: contentsOnly && info.IsDir()}, readPath, nil
}

// WriteArchive writes a copy archive of readPath described by src to w.
// Symlinks inside a directory are archived as links and never followed, and
// rooted file access keeps reads inside the directory even if it changes while
// it is archived. Sockets, devices, and FIFOs are skipped.
func WriteArchive(ctx context.Context, w io.Writer, readPath string, src Source, opts WriteOptions) error {
	if err := ValidateSource(src); err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, archiveBufferSize)
	tw := tar.NewWriter(bw)

	var err error
	if src.IsDir {
		err = writeDir(ctx, tw, readPath, src, opts)
	} else {
		err = writeFile(tw, readPath, src.Name)
	}
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return errx.Wrap(ErrArchive, err)
	}
	return errx.Wrap(ErrArchive, bw.Flush())
}

func writeFile(tw *tar.Writer, readPath, name string) error {
	info, err := os.Lstat(readPath)
	if err != nil {
		return errx.Wrap(ErrSource, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		link, err := os.Readlink(readPath)
		if err != nil {
			return errx.Wrap(ErrSource, err)
		}
		return writeHeader(tw, info, link, name)
	case info.Mode().IsRegular():
		file, err := os.OpenFile(readPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return errx.Wrap(ErrSource, err)
		}
		defer file.Close()
		return writeRegular(tw, file, name)
	default:
		return errx.With(ErrSource, ": %s: unsupported file type %s", readPath, info.Mode().Type())
	}
}

func writeDir(ctx context.Context, tw *tar.Writer, readPath string, src Source, opts WriteOptions) error {
	root, err := os.OpenRoot(readPath)
	if err != nil {
		return errx.Wrap(ErrSource, err)
	}
	defer root.Close()

	var matcher *patternmatcher.PatternMatcher
	if len(opts.Exclude) > 0 {
		matcher, err = patternmatcher.New(opts.Exclude)
		if err != nil {
			return errx.With(ErrSource, ": invalid exclude pattern: %w", err)
		}
	}

	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return errx.Wrap(ErrSource, walkErr)
		}
		archiveName := path.Join(src.Name, name)
		if name != "." && matcher != nil {
			excluded, err := matcher.MatchesOrParentMatches(name)
			if err != nil {
				return errx.Wrap(ErrSource, err)
			}
			if excluded {
				// Only descend when a negated pattern could re-include something.
				if entry.IsDir() && !mayIncludeDescendant(matcher, name) {
					return fs.SkipDir
				}
				return nil
			}
		}

		info, err := entry.Info()
		if err != nil {
			return errx.Wrap(ErrSource, err)
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if name == "." {
				// The root entry carries the directory's own metadata.
				info, err = root.Stat(".")
				if err != nil {
					return errx.Wrap(ErrSource, err)
				}
			}
			return writeHeader(tw, info, "", archiveName+"/")
		case mode&os.ModeSymlink != 0:
			link, err := root.Readlink(filepath.FromSlash(name))
			if err != nil {
				return errx.Wrap(ErrSource, err)
			}
			return writeHeader(tw, info, link, archiveName)
		case mode.IsRegular():
			file, err := root.OpenFile(filepath.FromSlash(name), os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return errx.Wrap(ErrSource, err)
			}
			defer file.Close()
			return writeRegular(tw, file, archiveName)
		default:
			skip(opts.OnSkip, archiveName, fmt.Sprintf("unsupported file type %s", mode.Type()))
			return nil
		}
	})
}

func writeRegular(tw *tar.Writer, file *os.File, name string) error {
	info, err := file.Stat()
	if err != nil {
		return errx.Wrap(ErrSource, err)
	}
	if !info.Mode().IsRegular() {
		return errx.With(ErrSource, ": %s: not a regular file", name)
	}
	if err := writeHeader(tw, info, "", name); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, file, info.Size()); err != nil {
		if errors.Is(err, io.EOF) {
			return errx.With(ErrSource, ": %s: file changed while copying", name)
		}
		return errx.Wrap(ErrArchive, err)
	}
	return nil
}

func writeHeader(tw *tar.Writer, info fs.FileInfo, link, name string) error {
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return errx.Wrap(ErrArchive, err)
	}
	header.Name = name
	// Keep numeric ownership for archive mode, but not host account names.
	header.Uname, header.Gname = "", ""
	return errx.Wrap(ErrArchive, tw.WriteHeader(header))
}

// ValidateSource rejects source descriptions that could place entries outside
// the destination. A guest supplies the description when copying out, so it is
// not trusted.
func ValidateSource(src Source) error {
	switch {
	case src.Name == ".":
		if !src.IsDir || !src.ContentsOnly {
			return errx.With(ErrArchive, ": root source must copy directory contents")
		}
	case src.Name == "", src.Name == "..", strings.ContainsAny(src.Name, "/\\"):
		return errx.With(ErrArchive, ": invalid source name %q", src.Name)
	}
	if src.ContentsOnly && !src.IsDir {
		return errx.With(ErrArchive, ": contents-only source must be a directory")
	}
	return nil
}

func mayIncludeDescendant(matcher *patternmatcher.PatternMatcher, dir string) bool {
	for _, pattern := range matcher.Patterns() {
		if !pattern.Exclusion() {
			continue
		}
		// A glob can only match beneath its literal prefix. Be conservative
		// after the first wildcard, including ** which may span directories.
		prefix := filepath.ToSlash(pattern.String())
		if i := strings.IndexAny(prefix, "*?[\\"); i >= 0 {
			prefix = prefix[:i]
		}
		if strings.HasPrefix(dir, prefix) || strings.HasPrefix(prefix, dir+"/") {
			return true
		}
	}
	return false
}

func skip(onSkip func(string, string), name, reason string) {
	if onSkip != nil {
		onSkip(name, reason)
	}
}
