package sdk

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

type AllowListUpdate struct {
	Added        []string
	Removed      []string
	AllowedHosts []string
}

func (c *Client) AllowListAdd(ctx context.Context, hosts ...string) (*AllowListUpdate, error) {
	normalized, err := normalizeAllowListHosts(hosts)
	if err != nil {
		return nil, err
	}

	params := map[string]interface{}{
		"hosts": normalized,
	}
	result, err := c.sendRequestCtx(ctx, "allow_list_add", params, nil)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Added        []string `json:"added"`
		AllowedHosts []string `json:"allowed_hosts"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, errx.Wrap(ErrParseAllowList, err)
	}
	return &AllowListUpdate{
		Added:        parsed.Added,
		AllowedHosts: parsed.AllowedHosts,
	}, nil
}

func (c *Client) AllowListDelete(ctx context.Context, hosts ...string) (*AllowListUpdate, error) {
	normalized, err := normalizeAllowListHosts(hosts)
	if err != nil {
		return nil, err
	}

	params := map[string]interface{}{
		"hosts": normalized,
	}
	result, err := c.sendRequestCtx(ctx, "allow_list_delete", params, nil)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Removed      []string `json:"removed"`
		AllowedHosts []string `json:"allowed_hosts"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, errx.Wrap(ErrParseAllowList, err)
	}
	return &AllowListUpdate{
		Removed:      parsed.Removed,
		AllowedHosts: parsed.AllowedHosts,
	}, nil
}

func normalizeAllowListHosts(hosts []string) ([]string, error) {
	normalized := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		for _, token := range strings.Split(host, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			if _, ok := seen[token]; ok {
				continue
			}
			seen[token] = struct{}{}
			normalized = append(normalized, token)
		}
	}
	if len(normalized) == 0 {
		return nil, ErrAllowListHosts
	}
	return normalized, nil
}

// ExecResult holds the result of command execution
