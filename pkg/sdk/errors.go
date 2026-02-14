package sdk

import "errors"

// Process / pipe errors (NewClient)
var (
	ErrStdinPipe  = errors.New("get stdin pipe")
	ErrStdoutPipe = errors.New("get stdout pipe")
	ErrStderrPipe = errors.New("get stderr pipe")
	ErrStartProc  = errors.New("start matchlock")
)

// Request lifecycle errors (sendRequestCtx, startReader)
var (
	ErrClientClosed    = errors.New("client is closed")
	ErrMarshalRequest  = errors.New("marshal request")
	ErrWriteRequest    = errors.New("write request")
	ErrConnectionClose = errors.New("connection closed")
)

// Create / VM errors
var (
	ErrImageRequired     = errors.New("image is required (e.g., alpine:latest)")
	ErrParseCreateResult = errors.New("parse create result")
	ErrInvalidVFSHook    = errors.New("invalid vfs hook")
	ErrVFSHookBlocked    = errors.New("vfs hook blocked operation")
	ErrParsePortForwards = errors.New("parse port-forward spec")
	ErrParsePortBindings = errors.New("parse port-forward result")
)

// Exec errors
var (
	ErrParseExecResult       = errors.New("parse exec result")
	ErrParseExecStreamResult = errors.New("parse exec_stream result")
)

// File operation errors
var (
	ErrParseReadResult = errors.New("parse read result")
	ErrParseListResult = errors.New("parse list result")
)

// Close / Remove errors
var (
	ErrCloseTimeout = errors.New("close timed out, process killed")
	ErrRemoveVM     = errors.New("matchlock rm")
)
