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
	errCreateClient      = errors.New("create client")
	errLaunchSandbox     = errors.New("launch sandbox")
	errExecRequestHook   = errors.New("exec request hook demo")
	errExecResponseHook  = errors.New("exec response hook demo")
	errAllowListRestrict = errors.New("restrict allow-list")
	errExecBlockedHost   = errors.New("exec blocked host check")
	errAllowListExpand   = errors.New("expand allow-list")
	errExecSSEHook       = errors.New("exec sse hook demo")
	errUnexpectedOutput  = errors.New("unexpected output")
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := sdk.DefaultConfig()
	if os.Getenv("MATCHLOCK_BIN") == "" {
		cfg.BinaryPath = "./bin/matchlock"
	}

	client, err := sdk.NewClient(cfg)
	if err != nil {
		return errx.Wrap(errCreateClient, err)
	}
	defer client.Remove()
	defer client.Close(0)

	sandbox := sdk.New("alpine:latest").
		WithNetworkInterception(&sdk.NetworkInterceptionConfig{
			Rules: []sdk.NetworkHookRule{
				{
					Name:          "request-mutate",
					Phase:         sdk.NetworkHookPhaseBefore,
					Action:        sdk.NetworkHookActionMutate,
					Hosts:         []string{"httpbin.org"},
					Methods:       []string{"GET"},
					Path:          "/anything/v1",
					SetHeaders:    map[string]string{"X-Hook": "set"},
					DeleteHeaders: []string{"X-Remove"},
					SetQuery:      map[string]string{"trace": "hooked"},
					DeleteQuery:   []string{"drop"},
					RewritePath:   "/anything/v2",
				},
				{
					Name:                  "response-mutate",
					Phase:                 sdk.NetworkHookPhaseAfter,
					Action:                sdk.NetworkHookActionMutate,
					Hosts:                 []string{"httpbin.org"},
					Path:                  "/response-headers",
					SetResponseHeaders:    map[string]string{"X-Intercepted": "true"},
					DeleteResponseHeaders: []string{"X-Upstream"},
					BodyReplacements: []sdk.NetworkBodyTransform{
						{Find: "foo", Replace: "bar"},
					},
				},
				{
					Name:   "sse-mutate",
					Phase:  sdk.NetworkHookPhaseAfter,
					Action: sdk.NetworkHookActionMutate,
					Hosts:  []string{"httpbingo.org"},
					Path:   "/sse",
					BodyReplacements: []sdk.NetworkBodyTransform{
						{Find: `"id"`, Replace: `"sid"`},
					},
				},
			},
		})

	vmID, err := client.Launch(sandbox)
	if err != nil {
		return errx.Wrap(errLaunchSandbox, err)
	}
	slog.Info("sandbox ready", "vm", vmID)

	ctx := context.Background()

	fmt.Println("Network interception walkthrough")
	fmt.Println("1) Request hook: rewrite path/query, set header, delete header")
	requestHookResult, err := client.Exec(ctx, `sh -c 'wget -q -O - --header "X-Remove: 1" "http://httpbin.org/anything/v1?drop=1"'`)
	if err != nil {
		return errx.Wrap(errExecRequestHook, err)
	}
	requestOut := requestHookResult.Stdout + requestHookResult.Stderr
	if err := mustContain(requestOut, "/anything/v2?trace=hooked", `"X-Hook": "set"`); err != nil {
		return errx.Wrap(errExecRequestHook, err)
	}
	if err := mustNotContain(requestOut, `"drop": "1"`, "X-Remove"); err != nil {
		return errx.Wrap(errExecRequestHook, err)
	}
	fmt.Println(clip(requestOut, 420))

	fmt.Println("\n2) Response hook: set/delete response headers + body replacement")
	responseHookResult, err := client.Exec(ctx, `sh -c 'wget -S -O - "http://httpbin.org/response-headers?X-Upstream=1&body=foo" 2>&1'`)
	if err != nil {
		return errx.Wrap(errExecResponseHook, err)
	}
	responseOut := responseHookResult.Stdout + responseHookResult.Stderr
	if err := mustContain(strings.ToLower(responseOut), "x-intercepted: true", `"body": "bar"`); err != nil {
		return errx.Wrap(errExecResponseHook, err)
	}
	if err := mustNotContain(strings.ToLower(responseOut), "x-upstream: 1"); err != nil {
		return errx.Wrap(errExecResponseHook, err)
	}
	fmt.Println(clip(responseOut, 420))

	fmt.Println("\n3) Runtime allow-list update: restrict to example.com (httpbin becomes blocked)")
	restricted, err := client.AllowListAdd(ctx, "example.com")
	if err != nil {
		return errx.Wrap(errAllowListRestrict, err)
	}
	slog.Info("allow-list restricted",
		"added", strings.Join(restricted.Added, ","),
		"current", formatHosts(restricted.AllowedHosts),
	)

	blockedHostResult, err := client.Exec(ctx, `sh -c 'wget -q -T 5 -O - http://httpbin.org/get 2>&1 || echo BLOCKED'`)
	if err != nil {
		return errx.Wrap(errExecBlockedHost, err)
	}
	blockedOut := blockedHostResult.Stdout + blockedHostResult.Stderr
	if err := mustContain(blockedOut, "BLOCKED"); err != nil {
		return errx.Wrap(errExecBlockedHost, err)
	}
	fmt.Println(clip(blockedOut, 240))

	fmt.Println("\n4) Expand allow-list and run SSE body replacement hook")
	expanded, err := client.AllowListAdd(ctx, "httpbin.org,httpbingo.org")
	if err != nil {
		return errx.Wrap(errAllowListExpand, err)
	}
	slog.Info("allow-list expanded",
		"added", strings.Join(expanded.Added, ","),
		"current", formatHosts(expanded.AllowedHosts),
	)

	sseResult, err := client.Exec(ctx, `sh -c 'wget -q -O - "http://httpbingo.org/sse?count=2" 2>&1'`)
	if err != nil {
		return errx.Wrap(errExecSSEHook, err)
	}
	sseOut := sseResult.Stdout + sseResult.Stderr
	if err := mustContain(sseOut, `data: {"sid":0`, `data: {"sid":1`); err != nil {
		return errx.Wrap(errExecSSEHook, err)
	}
	if err := mustNotContain(sseOut, `data: {"id":0`); err != nil {
		return errx.Wrap(errExecSSEHook, err)
	}
	fmt.Println(clip(sseOut, 360))

	fmt.Println("\nDone. Interception hooks and runtime allow-list updates are working.")

	return nil
}

func formatHosts(hosts []string) string {
	if len(hosts) == 0 {
		return "(empty: all hosts allowed)"
	}
	return strings.Join(hosts, ",")
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func mustContain(output string, values ...string) error {
	for _, v := range values {
		if strings.Contains(output, v) {
			continue
		}
		return errx.With(errUnexpectedOutput, ": missing %q", v)
	}
	return nil
}

func mustNotContain(output string, values ...string) error {
	for _, v := range values {
		if !strings.Contains(output, v) {
			continue
		}
		return errx.With(errUnexpectedOutput, ": unexpectedly found %q", v)
	}
	return nil
}
