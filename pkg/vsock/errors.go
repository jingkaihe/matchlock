package vsock

import "errors"

// Socket lifecycle errors
var (
	ErrCreateSocket = errors.New("create vsock socket")
	ErrBind         = errors.New("bind vsock")
	ErrListen       = errors.New("listen on vsock")
	ErrAccept       = errors.New("accept vsock connection")
	ErrConnect      = errors.New("connect to vsock")
)

// Device/CID errors
var (
	ErrOpenDevice  = errors.New("open /dev/vsock")
	ErrGetLocalCID = errors.New("get local CID")
)

// Wire protocol errors
var (
	ErrEncodeExecRequest  = errors.New("encode exec request")
	ErrWriteHeader        = errors.New("write header")
	ErrWriteRequest       = errors.New("write request")
	ErrReadResponseHeader = errors.New("read response header")
	ErrReadResponseData   = errors.New("read response data")

	ErrEncodePortForwardRequest = errors.New("encode port-forward request")
	ErrReadPortForwardResponse  = errors.New("read port-forward response")
	ErrPortForwardRejected      = errors.New("port-forward rejected")
	ErrUnexpectedPortForwardMsg = errors.New("unexpected port-forward response message")
)
