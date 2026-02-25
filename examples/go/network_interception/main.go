package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/sdk"
)

var (
	errCreateClient = errors.New("create client")
	errLaunchVM     = errors.New("launch sandbox")
	errRunDemo      = errors.New("run interception demo")
	errUnexpected   = errors.New("unexpected output")
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := sdk.DefaultConfig()

	client, err := sdk.NewClient(cfg)
	if err != nil {
		return errx.Wrap(errCreateClient, err)
	}
	defer client.Remove()
	defer client.Close(0)

	// One clear hook: mutate responses from /response-headers on httpbin.org.
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		WithNetworkInterception(&sdk.NetworkInterceptionConfig{
			Rules: []sdk.NetworkHookRule{
				{
					Name:  "dynamic-response-callback",
					Phase: sdk.NetworkHookPhaseAfter,
					Hosts: []string{"httpbin.org"},
					Path:  "/response-headers",
					Hook: func(ctx context.Context, req sdk.NetworkHookRequest) (*sdk.NetworkHookResult, error) {
						// This callback runs only when host/path/phase prefilters match.
						if req.StatusCode != 200 {
							return nil, nil
						}
						return &sdk.NetworkHookResult{
							Action: sdk.NetworkHookActionMutate,
							Response: &sdk.NetworkHookResponseMutation{
								Headers: map[string][]string{
									"X-Intercepted": []string{"callback"},
								},
								SetBody: []byte(`{"msg":"from-callback"}`),
							},
						}, nil
					},
				},
			},
		})

	vmID, err := client.Launch(sandbox)
	if err != nil {
		return errx.Wrap(errLaunchVM, err)
	}
	slog.Info("sandbox ready", "vm", vmID)

	// Upstream returns body=foo and header X-Upstream=1.
	// Callback hook should replace body, remove X-Upstream, and add X-Intercepted.
	result, err := client.Exec(
		context.Background(),
		`sh -c 'wget -S -O - "http://httpbin.org/response-headers?X-Upstream=1&body=foo" 2>&1'`,
	)
	if err != nil {
		return errx.Wrap(errRunDemo, err)
	}

	out := result.Stdout + result.Stderr
	fmt.Println(out)

	lower := strings.ToLower(out)
	if !strings.Contains(out, `{"msg":"from-callback"}`) {
		return errx.With(errUnexpected, `: expected callback to replace response body`)
	}
	if strings.Contains(lower, "x-upstream: 1") {
		return errx.With(errUnexpected, `: expected header "X-Upstream" to be removed`)
	}
	if !strings.Contains(lower, "x-intercepted: callback") {
		return errx.With(errUnexpected, `: expected header "X-Intercepted: callback"`)
	}

	fmt.Println("OK: callback hook intercepted and mutated the response")
	return nil
}
