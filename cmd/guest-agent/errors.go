//go:build linux

package main

import "errors"

var (
	// Vsock errors
	ErrSocket  = errors.New("socket")
	ErrBind    = errors.New("bind")
	ErrListen  = errors.New("listen")
	ErrConnect = errors.New("connect")
	ErrEOF     = errors.New("EOF")

	// User resolution errors
	ErrResolveUID   = errors.New("resolve uid")
	ErrResolveGID   = errors.New("resolve gid")
	ErrUserNotFound  = errors.New("user not found")
	ErrGroupNotFound = errors.New("group not found")
)
