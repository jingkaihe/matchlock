package copyarchive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// PlanKind says how a copy archive is placed at its destination.
type PlanKind int

const (
	// PlanIntoDir extracts entries into the existing directory Dir.
	PlanIntoDir PlanKind = iota + 1
	// PlanNewDir creates the directory Name inside Dir from a directory source.
	PlanNewDir
	// PlanFile writes a single-entry source as Name inside Dir, replacing any
	// existing file.
	PlanFile
)

// Plan places a copy archive at its destination.
type Plan struct {
	Kind PlanKind
	// Dir is an existing directory on the destination side.
	Dir string
	// Name is the entry created inside Dir for PlanNewDir and PlanFile.
	Name string
	// Source describes the archive root, or is nil for a raw archive that is
	// extracted as-is.
	Source *Source
}

// PlanDestination applies the copy path rules to dest for an archive described
// by src:
//
//   - An existing directory receives the source inside it, or the source's
//     contents when the source path ended in "/.".
//   - A file source replaces an existing file, or is created under the new
//     name when dest does not exist.
//   - A directory source creates dest when it does not exist.
//   - A trailing "/" on dest requires a directory, which must already exist
//     for a file source.
//
// The parent of a destination that does not exist must already exist. A nil
// src describes a raw archive, which requires dest to be an existing
// directory.
func PlanDestination(dest string, src *Source) (Plan, error) {
	if dest == "" {
		return Plan{}, errx.With(ErrDestination, ": path is empty")
	}
	if src != nil {
		if err := ValidateSource(*src); err != nil {
			return Plan{}, err
		}
	}
	requireDir := strings.HasSuffix(dest, "/") || strings.HasSuffix(dest, "/.")
	cleaned := filepath.Clean(dest)

	info, err := os.Stat(cleaned)
	switch {
	case err == nil && info.IsDir():
		return Plan{Kind: PlanIntoDir, Dir: cleaned, Source: src}, nil
	case err == nil:
		if requireDir {
			return Plan{}, errx.With(ErrDestination, ": %s: not a directory", dest)
		}
		if src == nil {
			return Plan{}, errx.With(ErrDestination, ": %s: archive destination must be a directory", dest)
		}
		if src.IsDir {
			return Plan{}, errx.With(ErrDestination, ": cannot copy a directory to file %s", dest)
		}
		return Plan{Kind: PlanFile, Dir: filepath.Dir(cleaned), Name: filepath.Base(cleaned), Source: src}, nil
	case !errors.Is(err, os.ErrNotExist):
		return Plan{}, errx.Wrap(ErrDestination, err)
	}

	if src == nil {
		return Plan{}, errx.With(ErrDestination, ": %s: directory does not exist", dest)
	}
	if requireDir && !src.IsDir {
		return Plan{}, errx.With(ErrDestination, ": %s: directory does not exist", dest)
	}
	parent := filepath.Dir(cleaned)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return Plan{}, errx.Wrap(ErrDestination, err)
	}
	if !parentInfo.IsDir() {
		return Plan{}, errx.With(ErrDestination, ": %s: not a directory", parent)
	}
	kind := PlanFile
	if src.IsDir {
		kind = PlanNewDir
	}
	return Plan{Kind: kind, Dir: parent, Name: filepath.Base(cleaned), Source: src}, nil
}
