//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errNewClient = errors.New("NewClient")
	errLaunch    = errors.New("Launch")
)

func TestConcurrentSandboxesWithProxy(t *testing.T) {
	const n = 3

	sandbox := func() *sdk.SandboxBuilder {
		return sdk.New("alpine:latest").
			WithCPUs(acceptanceDefaultCPUs).
			AllowHost("httpbin.org")
	}

	var (
		mu      sync.Mutex
		clients []*sdk.Client
	)

	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			client, err := sdk.NewClient(matchlockConfig(t))
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: %w: %w", idx, errNewClient, err)
				return
			}

			mu.Lock()
			clients = append(clients, client)
			mu.Unlock()

			_, err = client.Launch(sandbox())
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: %w: %w", idx, errLaunch, err)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range clients {
			c.Close(0)
			c.Remove()
		}
	})

	for err := range errs {
		require.NoError(t, err)
	}

	mu.Lock()
	activeClients := make([]*sdk.Client, len(clients))
	copy(activeClients, clients)
	mu.Unlock()

	require.Len(t, activeClients, n)

	for i, client := range activeClients {
		result, err := client.Exec(context.Background(), "echo hello")
		if !assert.NoErrorf(t, err, "sandbox %d: Exec", i) {
			continue
		}
		assert.Equalf(t, "hello", strings.TrimSpace(result.Stdout), "sandbox %d: stdout", i)
	}
}

func TestConcurrentSandboxesHTTPRequest(t *testing.T) {
	const n = 2

	var (
		mu      sync.Mutex
		clients []*sdk.Client
	)

	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			sb := sdk.New("alpine:latest").
				WithCPUs(acceptanceDefaultCPUs).
				AllowHost("httpbin.org")

			client, err := sdk.NewClient(matchlockConfig(t))
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: %w: %w", idx, errNewClient, err)
				return
			}

			mu.Lock()
			clients = append(clients, client)
			mu.Unlock()

			_, err = client.Launch(sb)
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: %w: %w", idx, errLaunch, err)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range clients {
			c.Close(0)
			c.Remove()
		}
	})

	for err := range errs {
		require.NoError(t, err)
	}

	mu.Lock()
	activeClients := make([]*sdk.Client, len(clients))
	copy(activeClients, clients)
	mu.Unlock()

	for i, client := range activeClients {
		result, err := client.Exec(context.Background(), "wget -q -O - https://httpbin.org/get 2>&1")
		if !assert.NoErrorf(t, err, "sandbox %d: Exec", i) {
			continue
		}
		combined := result.Stdout + result.Stderr
		assert.Containsf(t, combined, `"url"`, "sandbox %d: expected httpbin.org response", i)
	}
}

func TestConcurrentSandboxesWithSecrets(t *testing.T) {
	const n = 2

	secrets := []string{
		"sk-concurrent-secret-aaa",
		"sk-concurrent-secret-bbb",
	}

	clients := make([]*sdk.Client, n)

	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			sb := sdk.New("alpine:latest").
				WithCPUs(acceptanceDefaultCPUs).
				AllowHost("httpbin.org").
				AddSecret("MY_KEY", secrets[idx], "httpbin.org")

			client, err := sdk.NewClient(matchlockConfig(t))
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: %w: %w", idx, errNewClient, err)
				return
			}

			clients[idx] = client

			_, err = client.Launch(sb)
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: %w: %w", idx, errLaunch, err)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	t.Cleanup(func() {
		for _, c := range clients {
			if c != nil {
				c.Close(0)
				c.Remove()
			}
		}
	})

	for err := range errs {
		require.NoError(t, err)
	}

	for i, client := range clients {
		result, err := client.Exec(context.Background(), `sh -c 'wget -q -O - --header "Authorization: Bearer $MY_KEY" https://httpbin.org/headers 2>&1'`)
		if !assert.NoErrorf(t, err, "sandbox %d: Exec", i) {
			continue
		}
		assert.Containsf(t, result.Stdout, secrets[i], "sandbox %d: expected secret in response", i)
	}
}
