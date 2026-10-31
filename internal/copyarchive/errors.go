package copyarchive

import "errors"

var (
	ErrSource      = errors.New("copy source")
	ErrDestination = errors.New("copy destination")
	ErrArchive     = errors.New("copy archive")
	ErrLimit       = errors.New("copy limit exceeded")
)
