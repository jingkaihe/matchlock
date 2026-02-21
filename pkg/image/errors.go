package image

import "errors"

var (
	ErrParseReference = errors.New("parse image reference")
	ErrPullImage      = errors.New("pull image")
	ErrImageDigest    = errors.New("get image digest")
	ErrCreateDir      = errors.New("create directory")
	ErrCreateTemp     = errors.New("create temp")
	ErrExtract        = errors.New("extract image")
	ErrCreateExt4     = errors.New("create ext4")
	ErrToolNotFound   = errors.New("tool not found")
	ErrTarball        = errors.New("tarball")
	ErrStoreSave      = errors.New("save to store")
	ErrStoreRead      = errors.New("read from store")
	ErrMetadata       = errors.New("metadata")
	ErrImageNotFound  = errors.New("image not found")
	ErrLayerFS        = errors.New("invalid layer filesystem type")
)
