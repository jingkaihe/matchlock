package kernel

import "errors"

var (
	ErrDownloadKernel    = errors.New("download kernel")
	ErrParseReference    = errors.New("parse reference")
	ErrParsePlatform     = errors.New("parse platform")
	ErrGetDescriptor     = errors.New("get image descriptor")
	ErrGetImage          = errors.New("get image")
	ErrGetLayers         = errors.New("get layers")
	ErrNoLayers          = errors.New("no layers in image")
	ErrUncompressLayer   = errors.New("get uncompressed layer")
	ErrCreateDirectory   = errors.New("create directory")
	ErrReadLayer         = errors.New("read layer")
	ErrWriteKernel       = errors.New("write kernel")
	ErrRenameKernel      = errors.New("rename kernel")
	ErrCreateFile        = errors.New("create file")
	ErrKernelNotFound    = errors.New("kernel file not found in archive")
)
