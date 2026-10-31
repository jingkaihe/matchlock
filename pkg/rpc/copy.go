package rpc

import (
	"context"
	"encoding/json"
	"path"
	"path/filepath"

	"github.com/jingkaihe/matchlock/internal/copyarchive"
)

type copyParams struct {
	Src        string   `json:"src"`
	Dest       string   `json:"dest"`
	FollowLink bool     `json:"follow_link,omitempty"`
	Exclude    []string `json:"exclude,omitempty"`
	// copy_in
	Owner   string `json:"owner,omitempty"`
	Archive bool   `json:"archive,omitempty"`
	// copy_out: omitted uses the default limit, and 0 disables it.
	MaxSize  *int64 `json:"max_size,omitempty"`
	MaxFiles *int64 `json:"max_files,omitempty"`
}

// handleCopyIn copies a host path, which must be absolute, into the guest.
func (h *Handler) handleCopyIn(ctx context.Context, req *Request) *Response {
	guest, params, resp := h.copyRequest(req, "src")
	if resp != nil {
		return resp
	}
	if params.Owner != "" && params.Archive {
		return invalidParams(req, "owner cannot be combined with archive")
	}
	err := copyarchive.ToGuest(ctx, guest, params.Src, params.Dest, copyarchive.ToGuestOptions{
		Owner:         params.Owner,
		PreserveOwner: params.Archive,
		FollowLink:    params.FollowLink,
		Exclude:       params.Exclude,
	})
	return copyResponse(ctx, req, err)
}

// handleCopyOut copies a guest path to a host path, which must be absolute.
func (h *Handler) handleCopyOut(ctx context.Context, req *Request) *Response {
	guest, params, resp := h.copyRequest(req, "dest")
	if resp != nil {
		return resp
	}
	maxBytes, maxEntries := copyarchive.DefaultMaxBytes, copyarchive.DefaultMaxEntries
	if params.MaxSize != nil {
		maxBytes = *params.MaxSize
	}
	if params.MaxFiles != nil {
		maxEntries = *params.MaxFiles
	}
	if maxBytes < 0 || maxEntries < 0 {
		return invalidParams(req, "max_size and max_files must not be negative")
	}
	err := copyarchive.FromGuest(ctx, guest, params.Src, params.Dest, copyarchive.FromGuestOptions{
		FollowLink: params.FollowLink,
		Exclude:    params.Exclude,
		MaxBytes:   maxBytes,
		MaxEntries: maxEntries,
	})
	return copyResponse(ctx, req, err)
}

// copyRequest decodes copy params and checks that hostField names an absolute
// host path and the other field an absolute sandbox path.
func (h *Handler) copyRequest(req *Request, hostField string) (copyarchive.Guest, copyParams, *Response) {
	var params copyParams
	vm := h.getVM()
	if vm == nil {
		return nil, params, &Response{JSONRPC: "2.0", Error: &Error{Code: ErrCodeVMFailed, Message: "VM not created"}, ID: req.ID}
	}
	guest, ok := vm.(copyarchive.Guest)
	if !ok {
		return nil, params, &Response{JSONRPC: "2.0", Error: &Error{Code: ErrCodeVMFailed, Message: "VM does not support copying files"}, ID: req.ID}
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, params, invalidParams(req, err.Error())
	}
	hostPath, guestPath, guestField := params.Src, params.Dest, "dest"
	if hostField == "dest" {
		hostPath, guestPath, guestField = params.Dest, params.Src, "src"
	}
	switch {
	case params.Src == "" || params.Dest == "":
		return nil, params, invalidParams(req, "src and dest are required")
	case !filepath.IsAbs(hostPath):
		return nil, params, invalidParams(req, hostField+" must be an absolute host path")
	case !path.IsAbs(guestPath):
		return nil, params, invalidParams(req, guestField+" must be an absolute sandbox path")
	}
	return guest, params, nil
}

func copyResponse(ctx context.Context, req *Request, err error) *Response {
	if err == nil {
		return &Response{JSONRPC: "2.0", Result: map[string]interface{}{}, ID: req.ID}
	}
	code := ErrCodeFileFailed
	if ctx.Err() != nil {
		code = ErrCodeCancelled
	}
	return &Response{JSONRPC: "2.0", Error: &Error{Code: code, Message: err.Error()}, ID: req.ID}
}

func invalidParams(req *Request, message string) *Response {
	return &Response{JSONRPC: "2.0", Error: &Error{Code: ErrCodeInvalidParams, Message: message}, ID: req.ID}
}
