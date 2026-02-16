package guestfused

import "errors"

var (
	ErrSocket  = errors.New("socket")
	ErrConnect = errors.New("connect")
	ErrEOF     = errors.New("EOF")
)
