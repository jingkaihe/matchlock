// Package copyarchive implements the archive format and path rules used to
// copy files and directories between the host and a guest.
//
// A copy archive is a tar stream rooted at the source's base name: a file
// source is a single entry, and a directory source is the directory entry
// followed by its contents. The destination side decides where entries land,
// following docker cp rules (see PlanDestination).
package copyarchive

import (
	"context"
	"encoding/json"
	"io"
)

const (
	// DefaultMaxBytes is the default limit on file content written to the
	// host when copying out of a guest.
	DefaultMaxBytes int64 = 10 << 30
	// DefaultMaxEntries is the default limit on archive entries written to the
	// host when copying out of a guest.
	DefaultMaxEntries int64 = 1_000_000
)

// Source describes the root of a copy archive.
type Source struct {
	// Name is the source's base name. Every archive entry is Name or is
	// below Name. It is "." when the source is a filesystem root.
	Name string `json:"name"`
	// IsDir reports whether the source is a directory.
	IsDir bool `json:"is_dir"`
	// ContentsOnly copies the contents of a directory source rather than the
	// directory itself, as requested by a source path ending in "/.".
	ContentsOnly bool `json:"contents_only,omitempty"`
}

// CopyInRequest asks a guest to extract a copy archive.
type CopyInRequest struct {
	// Dest is the guest destination path.
	Dest string `json:"dest"`
	// Source describes the archive root. When nil, the archive is extracted
	// as-is into Dest, which must be an existing directory.
	Source *Source `json:"source,omitempty"`
	// Owner sets the owner of extracted entries: "uid", "uid:gid", "user", or
	// "user:group". Empty leaves them owned by root.
	Owner string `json:"owner,omitempty"`
	// PreserveOwner keeps the numeric owner recorded in the archive.
	PreserveOwner bool `json:"preserve_owner,omitempty"`
}

// CopyOutRequest asks a guest to stream a copy archive of Path.
type CopyOutRequest struct {
	Path       string   `json:"path"`
	FollowLink bool     `json:"follow_link,omitempty"`
	Exclude    []string `json:"exclude,omitempty"`
	// OnSkip is called on the host for entries the guest leaves out of the
	// archive. It is not sent to the guest.
	OnSkip func(name, reason string) `json:"-"`
}

// skipNotice reports an entry left out of a copy archive.
type skipNotice struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// EncodeSkip encodes a skip notice sent alongside a copy archive.
func EncodeSkip(name, reason string) []byte {
	data, _ := json.Marshal(skipNotice{Name: name, Reason: reason})
	return data
}

// ReportSkip passes an encoded skip notice to onSkip, ignoring malformed ones.
func ReportSkip(onSkip func(name, reason string), data []byte) {
	var notice skipNotice
	if onSkip != nil && json.Unmarshal(data, &notice) == nil {
		onSkip(notice.Name, notice.Reason)
	}
}

// Guest is implemented by transports that copy archives into and out of a
// running guest.
type Guest interface {
	// CopyIn streams archive to the guest and returns once it is extracted.
	CopyIn(ctx context.Context, req CopyInRequest, archive io.Reader) error
	// CopyOut returns the source description and a reader for its archive.
	// The reader reports a guest-side failure instead of io.EOF. Closing it
	// aborts the transfer.
	CopyOut(ctx context.Context, req CopyOutRequest) (Source, io.ReadCloser, error)
}
