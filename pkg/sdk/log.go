package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// Log returns the buffered VM log content.
func (c *Client) Log(ctx context.Context) (string, error) {
	result, err := c.sendRequestCtx(ctx, "log", nil, nil)
	if err != nil {
		return "", err
	}

	var logResult struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result, &logResult); err != nil {
		return "", errx.Wrap(ErrParseLogResult, err)
	}

	data, err := base64.StdEncoding.DecodeString(logResult.Content)
	if err != nil {
		return "", errx.Wrap(ErrParseLogResult, err)
	}
	return string(data), nil
}

// LogStream writes the current VM log to stdout and continues streaming newly
// appended data until ctx is cancelled.
func (c *Client) LogStream(ctx context.Context, stdout io.Writer) error {
	onNotification := func(method string, params json.RawMessage) {
		if method != "log_stream.data" || stdout == nil {
			return
		}

		var chunk struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(params, &chunk); err != nil {
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			return
		}
		_, _ = stdout.Write(decoded)
	}

	_, err := c.sendRequestCtx(ctx, "log_stream", nil, onNotification)
	return err
}
