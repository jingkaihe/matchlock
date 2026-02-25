package sdk

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

type VolumeInfo struct {
	Name string
	Size string
	Path string
}

// VolumeCreate creates a named raw ext4 volume and returns its metadata.
func (c *Client) VolumeCreate(name string, sizeMB int) (*VolumeInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrVolumeNameRequired
	}
	if sizeMB <= 0 {
		return nil, ErrInvalidVolumeSize
	}

	out, err := c.runCLICommand("volume", "create", name, "--size", strconv.Itoa(sizeMB), "--json")
	if err != nil {
		return nil, errx.With(ErrVolumeCommand, " create %s: %s: %w", name, strings.TrimSpace(string(out)), err)
	}

	info, err := parseVolumeCreateOutput(string(out))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(info.Name) == "" {
		info.Name = name
	}
	if strings.TrimSpace(info.Size) == "" {
		info.Size = fmt.Sprintf("%.1f MB", float64(sizeMB))
	}
	return &info, nil
}

// VolumeList returns all named raw ext4 volumes.
func (c *Client) VolumeList() ([]VolumeInfo, error) {
	out, err := c.runCLICommand("volume", "ls", "--json")
	if err != nil {
		return nil, errx.With(ErrVolumeCommand, " ls: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return parseVolumeListOutput(string(out))
}

// VolumeRemove removes a named raw ext4 volume.
func (c *Client) VolumeRemove(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrVolumeNameRequired
	}

	out, err := c.runCLICommand("volume", "rm", name)
	if err != nil {
		return errx.With(ErrVolumeCommand, " rm %s: %s: %w", name, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (c *Client) runCLICommand(args ...string) ([]byte, error) {
	bin := strings.TrimSpace(c.binaryPath)
	if bin == "" {
		return nil, ErrBinaryPathRequired
	}
	return exec.Command(bin, args...).CombinedOutput()
}

func parseVolumeCreateOutput(stdout string) (VolumeInfo, error) {
	var info VolumeInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &info); err != nil {
		return VolumeInfo{}, errx.Wrap(ErrParseVolumeCreateResult, err)
	}
	if strings.TrimSpace(info.Path) == "" {
		return VolumeInfo{}, ErrParseVolumeCreateResult
	}
	return info, nil
}

func parseVolumeListOutput(stdout string) ([]VolumeInfo, error) {
	var volumes []VolumeInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &volumes); err != nil {
		return nil, errx.Wrap(ErrParseVolumeListResult, err)
	}
	for _, v := range volumes {
		if strings.TrimSpace(v.Name) == "" || strings.TrimSpace(v.Path) == "" {
			return nil, ErrParseVolumeListResult
		}
	}
	return volumes, nil
}

// CreateOptions holds options for creating a sandbox
