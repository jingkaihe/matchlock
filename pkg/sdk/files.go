package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/jingkaihe/matchlock/internal/errx"
)

func (c *Client) WriteFile(ctx context.Context, path string, content []byte) error {
	return c.WriteFileMode(ctx, path, content, 0644)
}

// WriteFileMode writes content to a file with specific permissions.
func (c *Client) WriteFileMode(ctx context.Context, path string, content []byte, mode uint32) error {
	if err := c.applyLocalActionHooks(ctx, VFSHookOpWrite, path, len(content), mode); err != nil {
		return err
	}

	mutated, err := c.applyLocalWriteMutations(ctx, path, content, mode)
	if err != nil {
		return err
	}

	params := map[string]interface{}{
		"path":    path,
		"content": base64.StdEncoding.EncodeToString(mutated),
		"mode":    mode,
	}

	_, err = c.sendRequestCtx(ctx, "write_file", params, nil)
	return err
}

// ReadFile reads a file from the sandbox.
func (c *Client) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := c.applyLocalActionHooks(ctx, VFSHookOpRead, path, 0, 0); err != nil {
		return nil, err
	}

	params := map[string]string{
		"path": path,
	}

	result, err := c.sendRequestCtx(ctx, "read_file", params, nil)
	if err != nil {
		return nil, err
	}

	var readResult struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result, &readResult); err != nil {
		return nil, errx.Wrap(ErrParseReadResult, err)
	}

	return base64.StdEncoding.DecodeString(readResult.Content)
}

// FileInfo holds file metadata
type FileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"is_dir"`
}

// ListFiles lists files in a directory.
func (c *Client) ListFiles(ctx context.Context, path string) ([]FileInfo, error) {
	if err := c.applyLocalActionHooks(ctx, VFSHookOpReadDir, path, 0, 0); err != nil {
		return nil, err
	}

	params := map[string]string{
		"path": path,
	}

	result, err := c.sendRequestCtx(ctx, "list_files", params, nil)
	if err != nil {
		return nil, err
	}

	var listResult struct {
		Files []FileInfo `json:"files"`
	}
	if err := json.Unmarshal(result, &listResult); err != nil {
		return nil, errx.Wrap(ErrParseListResult, err)
	}

	return listResult.Files, nil
}
