//go:build acceptance

package acceptance

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/sdk"
)

func TestConcurrentSandboxesWithProxy(t *testing.T) {
	const n = 3

	sandbox := func() *sdk.SandboxBuilder {
		return sdk.New("alpine:latest").
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
				errs <- fmt.Errorf("sandbox %d: NewClient: %w", idx, err)
				return
			}

			mu.Lock()
			clients = append(clients, client)
			mu.Unlock()

			_, err = client.Launch(sandbox())
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: Launch: %w", idx, err)
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
		t.Fatal(err)
	}

	mu.Lock()
	activeClients := make([]*sdk.Client, len(clients))
	copy(activeClients, clients)
	mu.Unlock()

	if len(activeClients) != n {
		t.Fatalf("expected %d clients, got %d", n, len(activeClients))
	}

	for i, client := range activeClients {
		result, err := client.Exec("echo hello")
		if err != nil {
			t.Errorf("sandbox %d: Exec: %v", i, err)
			continue
		}
		if got := strings.TrimSpace(result.Stdout); got != "hello" {
			t.Errorf("sandbox %d: stdout = %q, want %q", i, got, "hello")
		}
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
				AllowHost("httpbin.org")

			client, err := sdk.NewClient(matchlockConfig(t))
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: NewClient: %w", idx, err)
				return
			}

			mu.Lock()
			clients = append(clients, client)
			mu.Unlock()

			_, err = client.Launch(sb)
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: Launch: %w", idx, err)
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
		t.Fatal(err)
	}

	mu.Lock()
	activeClients := make([]*sdk.Client, len(clients))
	copy(activeClients, clients)
	mu.Unlock()

	for i, client := range activeClients {
		result, err := client.Exec("wget -q -O - https://httpbin.org/get 2>&1")
		if err != nil {
			t.Errorf("sandbox %d: Exec: %v", i, err)
			continue
		}
		combined := result.Stdout + result.Stderr
		if !strings.Contains(combined, `"url"`) {
			t.Errorf("sandbox %d: expected httpbin.org response, got: %s", i, combined)
		}
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
				AllowHost("httpbin.org").
				AddSecret("MY_KEY", secrets[idx], "httpbin.org")

			client, err := sdk.NewClient(matchlockConfig(t))
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: NewClient: %w", idx, err)
				return
			}

			clients[idx] = client

			_, err = client.Launch(sb)
			if err != nil {
				errs <- fmt.Errorf("sandbox %d: Launch: %w", idx, err)
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
		t.Fatal(err)
	}

	for i, client := range clients {
		result, err := client.Exec(`sh -c 'wget -q -O - --header "Authorization: Bearer $MY_KEY" https://httpbin.org/headers 2>&1'`)
		if err != nil {
			t.Errorf("sandbox %d: Exec: %v", i, err)
			continue
		}
		if !strings.Contains(result.Stdout, secrets[i]) {
			t.Errorf("sandbox %d: expected secret %q in response, got: %s", i, secrets[i], result.Stdout)
		}
	}
}
