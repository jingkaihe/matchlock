package logging

import "errors"

var (
	ErrCreateLogFile = errors.New("logging: create log file")
	ErrWriteEvent    = errors.New("logging: write event")
	ErrMarshalData   = errors.New("logging: marshal event data")
	ErrCloseWriter   = errors.New("logging: close writer")
)
