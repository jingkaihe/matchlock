package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// writeBuildContext packages only the selected inputs. Symlinks are archived as
// links, never traversed, and rooted file access prevents a concurrent symlink
// replacement from making us read outside the context directory.
func writeBuildContext(ctx context.Context, w io.Writer, contextDir, dockerfile string) error {
	root, err := os.OpenRoot(contextDir)
	if err != nil {
		return err
	}
	defer root.Close()

	dockerRoot, err := os.OpenRoot(filepath.Dir(dockerfile))
	if err != nil {
		return err
	}
	defer dockerRoot.Close()
	ignoreData, err := readBuildIgnore(dockerRoot, filepath.Base(dockerfile)+".dockerignore")
	if errors.Is(err, os.ErrNotExist) {
		ignoreData, err = readBuildIgnore(root, ".dockerignore")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	patterns, err := ignorefile.ReadAll(bytes.NewReader(ignoreData))
	if err != nil {
		return err
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return err
	}

	tw := tar.NewWriter(w)
	for _, dir := range []string{"context", "dockerfile"} {
		if err := tw.WriteHeader(&tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
			return err
		}
	}

	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		ignored, err := matcher.MatchesOrParentMatches(name)
		if err != nil {
			return err
		}
		if ignored {
			// Only descend when a negated pattern could re-include something here.
			if entry.IsDir() && !mayIncludeBuildDescendant(matcher, name) {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		// Unix sockets are host runtime endpoints, not transferable build inputs.
		// Skip them by file type, without opening or connecting to them.
		if info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = root.Readlink(filepath.FromSlash(name))
			if err != nil {
				return err
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return errx.With(ErrBuildContext, ": unsupported file %q (%s)", name, info.Mode())
		}
		var file *os.File
		if info.Mode().IsRegular() {
			file, err = root.OpenFile(filepath.FromSlash(name), os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			defer file.Close()
			info, err = file.Stat()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return errx.With(ErrBuildContext, ": not a regular file: %q", name)
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = path.Join("context", name)
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if file != nil {
			_, err = io.CopyN(tw, file, info.Size())
		}
		return err
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Always send the explicitly selected Dockerfile separately, even if it is
	// outside the context or excluded by an ignore rule. Use a fixed guest name
	// so host filenames never become shell syntax in the build command.
	file, err := dockerRoot.OpenFile(filepath.Base(dockerfile), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errx.With(ErrBuildContext, ": Dockerfile must be a regular file")
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "dockerfile/Dockerfile", Mode: 0644, Size: info.Size(), ModTime: info.ModTime(),
	}); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, file, info.Size()); err != nil {
		return err
	}
	// BuildKit must use the same rules as the upload, including precedence of
	// Dockerfile-specific rules over the context's .dockerignore.
	if err := tw.WriteHeader(&tar.Header{
		Name: "dockerfile/Dockerfile.dockerignore", Mode: 0644, Size: int64(len(ignoreData)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(ignoreData); err != nil {
		return err
	}
	return tw.Close()
}

func mayIncludeBuildDescendant(matcher *patternmatcher.PatternMatcher, dir string) bool {
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

func readBuildIgnore(root *os.Root, name string) ([]byte, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errx.With(ErrBuildContext, ": ignore file must be a regular file: %q", name)
	}
	return io.ReadAll(file)
}
