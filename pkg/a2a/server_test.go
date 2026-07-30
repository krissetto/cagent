package a2a

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func TestRoutableAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		addr string
		want string
	}{
		{"ipv6 wildcard", "[::]:8080", "localhost:8080"},
		{"ipv4 wildcard", "0.0.0.0:8080", "localhost:8080"},
		{"empty host", ":8080", "localhost:8080"},
		{"localhost stays", "localhost:8080", "localhost:8080"},
		{"ipv4 loopback stays", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"specific ip stays", "192.168.1.1:9090", "192.168.1.1:9090"},
		{"hostname stays", "my-host:8080", "my-host:8080"},
		{"invalid addr returned as-is", "not-a-host-port", "not-a-host-port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, routableAddr(tt.addr))
		})
	}
}

// TestRun_StopsOnContextCancel: canceling the context must make Run stop
// serving and return, releasing the session store's file handles (otherwise
// t.TempDir cleanup fails on Windows with an open session.db).
func TestRun_StopsOnContextCancel(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	sessionDB := filepath.Join(t.TempDir(), "session.db")

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, "testdata/basic.yaml", "root", sessionDB, &config.RuntimeConfig{}, ln)
	}()

	// Cancel only once the server actually serves, so the test exercises a
	// mid-serve shutdown rather than a pre-start one.
	cardURL := "http://" + ln.Addr().String() + a2asrv.WellKnownAgentCardPath
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, http.NoBody)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 20*time.Millisecond, "A2A server never started serving")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "Run must return nil on context cancellation")
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
