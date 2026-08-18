package httpclient

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	desktoptransport "github.com/docker/docker-agent/pkg/desktop/transport"
)

// legacyDisableDesktopProxyEnv is the retired name that must remain inert.
const legacyDisableDesktopProxyEnv = "CAGENT_DISABLE_DESKTOP_PROXY"

func TestDesktopAwareTransportProxySafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		host     string
		resolver func(context.Context, string) ([]net.IP, error)
		want     bool
	}{
		{
			name: "literal private address",
			host: "127.0.0.1",
			want: false,
		},
		{
			name: "all resolved addresses private",
			host: "internal.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("192.168.1.1")}, nil
			},
			want: false,
		},
		{
			name: "mixed public and private addresses stay direct",
			host: "mixed.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("1.1.1.1")}, nil
			},
			want: false,
		},
		{
			name: "all resolved addresses public",
			host: "public.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")}, nil
			},
			want: true,
		},
		{
			name: "empty DNS response stays direct",
			host: "empty.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return nil, nil
			},
			want: false,
		},
		{
			name: "NXDOMAIN fails closed (Option A: broken resolver, not PAC-only network)",
			host: "proxy-only.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return nil, &net.DNSError{IsNotFound: true}
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			transport := newDesktopAwareTransport(true).(*desktopAwareTransport)
			if tt.resolver != nil {
				transport.resolver = tt.resolver
			}
			assert.Equal(t, tt.want, transport.proxySafe(t.Context(), tt.host))
		})
	}
}

func TestDesktopAwareTransportLoopbackIsNeverProxied(t *testing.T) {
	t.Parallel()

	assert.True(t, isLoopbackHost("localhost"))
	assert.True(t, isLoopbackHost("127.0.0.1"))
	assert.True(t, isLoopbackHost("::1"))
	assert.False(t, isLoopbackHost("example.com"))
}

func TestDesktopAwareTransportCachesDesktopTransport(t *testing.T) {
	desktoptransport.SetDesktopRunningForTest(t, func(context.Context) (bool, error) { return true, nil })

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	var factoryCalls int
	branch := &countingTransport{}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
		factoryCalls++
		return branch
	}

	for range 2 {
		resp := roundTrip(t, transport)
		require.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 2, branch.calls)
}

func TestDesktopAwareTransportDisabledOrGuardedUsesDirect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		guarded  bool
		host     string
		disabled bool
	}{
		{name: "kill switch", host: "example.com", disabled: true},
		{name: "guarded loopback", guarded: true, host: "127.0.0.1"},
		{name: "unguarded loopback", host: "127.0.0.1"},
		{name: "guarded non-Docker goes direct (allowlist)", guarded: true, host: "example.com"},
		{name: "guarded non-Docker arbitrary subdomain goes direct", guarded: true, host: "evil.docker.com.attacker.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.disabled {
				t.Setenv(disableDesktopProxyEnv, "1")
			}
			transport := newDesktopAwareTransport(tc.guarded).(*desktopAwareTransport)
			transport.resolver = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.1")}, nil }
			transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
				t.Fatal("Desktop transport must not be used")
				return nil
			}
			transport.direct = &http.Transport{}
			transport.direct.RegisterProtocol("https", roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
			}))
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+tc.host, http.NoBody)
			require.NoError(t, err)
			resp, err := transport.RoundTrip(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDesktopAwareTransportConsultsDesktopDetectionPerRequest(t *testing.T) {
	desktopRunning := false
	var detections int
	desktoptransport.SetDesktopRunningForTest(t, func(context.Context) (bool, error) {
		detections++
		return desktopRunning, nil
	})

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	direct := &countingTransport{}
	transport.direct = &http.Transport{}
	transport.direct.RegisterProtocol("https", direct)
	var factoryCalls int
	branch := &countingTransport{}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
		factoryCalls++
		return branch
	}

	for range 2 {
		resp := roundTrip(t, transport)
		require.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, 2, detections)
	assert.Equal(t, 2, direct.calls)
	assert.Zero(t, factoryCalls)

	desktopRunning = true
	for range 2 {
		resp := roundTrip(t, transport)
		require.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, 4, detections)
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 2, branch.calls)

	desktopRunning = false
	resp := roundTrip(t, transport)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 5, detections)
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 3, direct.calls)
}

func TestDesktopAwareTransportDisableCompressionBeforeDesktopTransport(t *testing.T) {
	desktoptransport.SetDesktopRunningForTest(t, func(context.Context) (bool, error) { return true, nil })

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	transport.direct = &http.Transport{}
	transport.direct.RegisterProtocol("https", roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}))
	transport.DisableCompression()
	proxy := &compressionTransport{}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper { return proxy }

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.True(t, proxy.disabled)
}

func TestDesktopAwareTransportDisableCompressionBeforeAndAfterDirectFallback(t *testing.T) {
	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	proxy := &compressionTransport{}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper { return proxy }

	transport.DisableCompression()
	assert.True(t, transport.direct.DisableCompression)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://127.0.0.1", http.NoBody)
	require.NoError(t, err)
	assert.True(t, isLoopbackHost(req.URL.Hostname()))
}

func TestDesktopAwareTransportRetainsDesktopBranchCooldown(t *testing.T) {
	desktoptransport.SetDesktopRunningForTest(t, func(context.Context) (bool, error) { return true, nil })

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	branch := &cooldownTransport{}
	var factoryCalls int
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
		factoryCalls++
		return branch
	}

	for range 2 {
		resp := roundTrip(t, transport)
		require.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 1, branch.proxyCalls)
	assert.Equal(t, 2, branch.directCalls)
}

func roundTrip(t *testing.T, transport http.RoundTripper) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	return resp
}

type countingTransport struct {
	calls int
}

func (t *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
}

type cooldownTransport struct {
	proxyCalls  int
	directCalls int
	disabled    bool
}

func (t *cooldownTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if !t.disabled {
		t.proxyCalls++
		t.disabled = true
	}
	t.directCalls++
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
}

type compressionTransport struct {
	disabled bool
}

func (t *compressionTransport) DisableCompression() {
	t.disabled = true
}

func (t *compressionTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
}

func TestIsDockerHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host     string
		expected bool
	}{
		{"docker.com", true},
		{"docker.io", true},
		{"DOCKER.COM", true},
		{"hub.docker.com", true},
		{"registry-1.docker.io", true},
		{"auth.docker.io", true},
		{"index.docker.io", true},
		{"cdn.registry.docker.io", true},
		{"desktop.docker.com", true},
		{"api.docker.com", true},
		{"docker.com.", true},
		{"hub.docker.com.", true},
		{"example.com", false},
		{"jenkins.internal", false},
		{"evil.localhost", false},
		{"notdocker.com", false},
		{"docker.com.attacker.com", false},
		{"evil.docker.com.attacker.com", false},
		{"fakedocker.com", false},
		{"xdocker.com", false},
		{"docker.org", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, isDockerHost(tt.host))
		})
	}
}

func TestDesktopAwareTransportUnguardedUsesDesktopForNonDockerHost(t *testing.T) {
	desktopHit := false
	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	transport.resolver = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("1.1.1.1")}, nil
	}
	transport.newDesktopTransport = func(_ context.Context, _ http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			desktopHit = true
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
		})
	}
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.True(t, desktopHit, "unguarded transport should route non-Docker host through Desktop")
}

func TestDesktopAwareTransportGuardedDockerHostUsesDesktop(t *testing.T) {
	desktopHit := false
	transport := newDesktopAwareTransport(true).(*desktopAwareTransport)
	transport.resolver = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("52.0.0.1")}, nil
	}
	transport.newDesktopTransport = func(_ context.Context, _ http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			desktopHit = true
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
		})
	}
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.docker.com/v2/", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.True(t, desktopHit, "guarded transport should route Docker host through Desktop when it resolves to a public IP")
}

func TestDesktopAwareTransportGuardedDockerHostNXDOMAINStaysDirect(t *testing.T) {
	desktopRoundTrips := 0
	directRoundTrips := 0
	transport := newDesktopAwareTransport(true).(*desktopAwareTransport)
	transport.resolver = func(context.Context, string) ([]net.IP, error) {
		return nil, &net.DNSError{IsNotFound: true}
	}
	transport.newDesktopTransport = func(_ context.Context, _ http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			desktopRoundTrips++
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})
	}
	transport.direct = &http.Transport{}
	transport.direct.RegisterProtocol("https", roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		directRoundTrips++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}))
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.docker.com/v2/", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Zero(t, desktopRoundTrips, "Desktop RoundTrip must not be called when proxySafe returns false (NXDOMAIN)")
	assert.Equal(t, 1, directRoundTrips, "direct transport must handle the request when proxySafe returns false")
}

func TestDesktopAwareTransportGuardedDockerHostPrivateIPStaysDirect(t *testing.T) {
	desktopRoundTrips := 0
	directRoundTrips := 0
	transport := newDesktopAwareTransport(true).(*desktopAwareTransport)
	transport.resolver = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.0.0.1")}, nil
	}
	transport.newDesktopTransport = func(_ context.Context, _ http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			desktopRoundTrips++
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})
	}
	transport.direct = &http.Transport{}
	transport.direct.RegisterProtocol("https", roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		directRoundTrips++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}))
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.docker.com/v2/", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Zero(t, desktopRoundTrips, "Desktop RoundTrip must not be called when proxySafe returns false (private IP)")
	assert.Equal(t, 1, directRoundTrips, "direct transport must handle the request when proxySafe returns false")
}

func TestDesktopProxyDisabled(t *testing.T) {
	t.Setenv(disableDesktopProxyEnv, "")
	t.Setenv(legacyDisableDesktopProxyEnv, "")
	assert.False(t, desktopProxyDisabled())

	t.Setenv(disableDesktopProxyEnv, "1")
	assert.True(t, desktopProxyDisabled())

	t.Setenv(disableDesktopProxyEnv, "")
	t.Setenv(legacyDisableDesktopProxyEnv, "1")
	assert.False(t, desktopProxyDisabled())
}
