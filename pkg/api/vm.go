package api

import (
	"context"
	"io"
	"time"
)

type ExecOptions struct {
	WorkingDir string
	Env        map[string]string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	User       string // "uid", "uid:gid", or username — resolved in guest
}

type ExecResult struct {
	ExitCode   int           `json:"exit_code"`
	Stdout     []byte        `json:"stdout,omitempty"`
	Stderr     []byte        `json:"stderr,omitempty"`
	DurationMS int64         `json:"duration_ms"`
	Duration   time.Duration `json:"-"`
}

type FileInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    uint32    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
	IsDir   bool      `json:"is_dir"`
}

type VM interface {
	ID() string
	Config() *Config
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Exec(ctx context.Context, command string, opts *ExecOptions) (*ExecResult, error)
	WriteFile(ctx context.Context, path string, content []byte, mode uint32) error
	ReadFile(ctx context.Context, path string) ([]byte, error)
	ListFiles(ctx context.Context, path string) ([]FileInfo, error)
	Events() <-chan Event
	Close() error
}

type Event struct {
	Type      string        `json:"type"`
	Timestamp int64         `json:"timestamp"`
	Network   *NetworkEvent `json:"network,omitempty"`
	File      *FileEvent    `json:"file,omitempty"`
	Exec      *ExecEvent    `json:"exec,omitempty"`
}

type NetworkEvent struct {
	Method        string `json:"method"`
	URL           string `json:"url"`
	Host          string `json:"host"`
	StatusCode    int    `json:"status_code"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	DurationMS    int64  `json:"duration_ms"`
	Blocked       bool   `json:"blocked"`
	BlockReason   string `json:"block_reason,omitempty"`
}

type FileEvent struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode,omitempty"`
	UID  int    `json:"uid,omitempty"`
	GID  int    `json:"gid,omitempty"`
}

type ExecEvent struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
}
