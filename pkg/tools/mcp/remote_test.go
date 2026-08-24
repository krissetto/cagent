package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/upstream"
)

// TestSanitizeRemoteAddress verifies that URLs with embedded credentials
// (basic-auth userinfo, query-string secrets) collapse to a host-only
// string before reaching the `server.address` span attribute. The point
// is exfiltration safety: a URL like `https://user:token@host/?api_key=…`
// would otherwise be replicated verbatim into every CLIENT span and
// shipped to the trace backend.
func TestSanitizeRemoteAddress(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want string
	}{
		{name: "plain", url: "https://example.com/mcp", want: "example.com"},
		{name: "host with port", url: "https://example.com:8443/mcp", want: "example.com:8443"},
		{name: "userinfo stripped", url: "https://alice:s3cret@example.com/mcp", want: "example.com"},
		{name: "query stripped", url: "https://example.com/mcp?api_key=s3cret", want: "example.com"},
		{name: "userinfo and query stripped", url: "https://alice:s3cret@example.com:8443/mcp?api_key=x", want: "example.com:8443"},
		{name: "fragment stripped", url: "https://example.com/mcp#frag", want: "example.com"},
		{name: "hostless empty fallback", url: "not-a-url", want: ""},
		{name: "empty input", url: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeRemoteAddress(tc.url)
			assert.Equal(t, tc.want, got, "sanitizeRemoteAddress(%q)", tc.url)
		})
	}
}

func TestSanitizeURLForLog(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		"https://example.com:8443/mcp",
		sanitizeURLForLog("https://alice:s3cret@example.com:8443/mcp?api_key=secret#fragment"),
	)
	assert.Empty(t, sanitizeURLForLog("not-a-url"))
}

func TestRemoteClientCustomHeaders(t *testing.T) {
	t.Parallel()

	var capturedRequest *http.Request
	requestCaptured := make(chan bool, 1)

	// Create a test SSE server that captures the request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRequest = r

		// Send a minimal SSE response to satisfy the client
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: {\"uri\":\"/message\"}\n\n")

		select {
		case requestCaptured <- true:
		default:
		}
	}))
	defer server.Close()

	// Create remote client WITH custom headers
	expectedHeaders := map[string]string{
		"X-Test-Header": "test-value",
		"X-API-Key":     "secret-key-12345",
		"Authorization": "Bearer custom-token",
	}

	client := newRemoteClient(server.URL, "sse", expectedHeaders, NewInMemoryTokenStore(), nil, false, nil)

	// Try to initialize (which will make the HTTP request)
	// We don't care if it succeeds or fails, we just need it to make the request
	_, _ = client.Initialize(t.Context(), nil)

	// Wait for the request to be captured
	select {
	case <-requestCaptured:
		// Verify that custom headers were applied
		for key, expectedValue := range expectedHeaders {
			actualValue := capturedRequest.Header.Get(key)
			assert.Equal(t, expectedValue, actualValue,
				"Expected header %s to have value %q, but got %q",
				key, expectedValue, actualValue)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Server did not receive request within timeout")
	}
}

// TestRemoteClientHeadersWithStreamable verifies that custom headers work with streamable transport
func TestRemoteClientHeadersWithStreamable(t *testing.T) {
	t.Parallel()

	var capturedRequest *http.Request
	requestCaptured := make(chan bool, 1)

	// Create a test server for streamable transport. The go-sdk client
	// sends several JSON-RPC calls (server/discover, then initialize), so
	// echo each request's id — a response with a non-matching id would
	// leave the call awaiting forever. The mock deliberately answers every
	// non-notification request with the same minimal initialize-shaped body;
	// its bogus protocol version makes Initialize fail fast, which the test
	// ignores: only headers matter.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRequest = r

		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			// Notification: no response expected.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","result":{"protocolVersion":"1.0.0","capabilities":{},"serverInfo":{"name":"test","version":"1.0.0"}},"id":%s}`, req.ID)

		select {
		case requestCaptured <- true:
		default:
		}
	}))
	defer server.Close()

	// Create remote client WITH custom headers using streamable transport
	expectedHeaders := map[string]string{
		"X-Custom-Auth": "custom-auth-value",
	}

	client := newRemoteClient(server.URL, "streamable", expectedHeaders, NewInMemoryTokenStore(), nil, false, nil)

	// Try to initialize
	_, _ = client.Initialize(t.Context(), nil)

	// Wait for the request to be captured
	select {
	case <-requestCaptured:
		// Verify that custom headers were applied
		actualValue := capturedRequest.Header.Get("X-Custom-Auth")
		assert.Equal(t, "custom-auth-value", actualValue,
			"Expected header X-Custom-Auth to have value %q, but got %q",
			"custom-auth-value", actualValue)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Server did not receive request within timeout")
	}
}

// TestRemoteClientNoHeaders verifies that the client works correctly even with no headers
func TestRemoteClientNoHeaders(t *testing.T) {
	t.Parallel()

	var capturedRequest *http.Request
	requestCaptured := make(chan bool, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRequest = r

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: {\"uri\":\"/message\"}\n\n")

		select {
		case requestCaptured <- true:
		default:
		}
	}))
	defer server.Close()

	// Create remote client without custom headers (nil)
	client := newRemoteClient(server.URL, "sse", nil, NewInMemoryTokenStore(), nil, false, nil)

	_, _ = client.Initialize(t.Context(), nil)

	// Wait for request
	select {
	case <-requestCaptured:
		// Just verify we got the request - no custom headers should be present
		require.NotNil(t, capturedRequest, "Request should have been captured")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Server did not receive request within timeout")
	}
}

// TestRemoteClientEmptyHeaders verifies that the client works correctly with an empty map
func TestRemoteClientEmptyHeaders(t *testing.T) {
	t.Parallel()

	var capturedRequest *http.Request
	requestCaptured := make(chan bool, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRequest = r

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: {\"uri\":\"/message\"}\n\n")

		select {
		case requestCaptured <- true:
		default:
		}
	}))
	defer server.Close()

	// Create remote client with empty headers map
	client := newRemoteClient(server.URL, "sse", map[string]string{}, NewInMemoryTokenStore(), nil, false, nil)

	_, _ = client.Initialize(t.Context(), nil)

	// Wait for request
	select {
	case <-requestCaptured:
		// Just verify we got the request
		require.NotNil(t, capturedRequest, "Request should have been captured")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Server did not receive request within timeout")
	}
}

// TestOAuthHTTPClientWithHeaders_ScopesHeadersToMCPHost verifies that the
// OAuth HTTP client forwards configured custom headers to requests targeting
// the MCP server's own host — where protected-resource-metadata discovery is
// served (issue #3148) — but NOT to requests aimed at a different host, such
// as an authorization server advertised in the server's own metadata.
func TestOAuthHTTPClientWithHeaders_ScopesHeadersToMCPHost(t *testing.T) {
	t.Parallel()

	var mcpHostHeader, thirdPartyHeader string
	mcpHostHit := make(chan struct{}, 1)
	thirdPartyHit := make(chan struct{}, 1)

	mcpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mcpHostHeader = r.Header.Get("X-Grafana-URL")
		w.WriteHeader(http.StatusOK)
		mcpHostHit <- struct{}{}
	}))
	defer mcpServer.Close()

	thirdParty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdPartyHeader = r.Header.Get("X-Grafana-URL")
		w.WriteHeader(http.StatusOK)
		thirdPartyHit <- struct{}{}
	}))
	defer thirdParty.Close()

	headers := map[string]string{"X-Grafana-URL": "https://instance.grafana.net/"}
	client := oauthHTTPClientWithHeaders(mcpServer.URL, headers, true, nil)

	mcpReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, mcpServer.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(mcpReq)
	require.NoError(t, err)
	resp.Body.Close()
	<-mcpHostHit
	assert.Equal(t, "https://instance.grafana.net/", mcpHostHeader,
		"requests to the MCP server's own host must carry the configured header")

	thirdPartyReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, thirdParty.URL, http.NoBody)
	require.NoError(t, err)
	resp, err = client.Do(thirdPartyReq)
	require.NoError(t, err)
	resp.Body.Close()
	<-thirdPartyHit
	assert.Empty(t, thirdPartyHeader,
		"requests to a third-party host must NOT carry the configured header (credential-leak guard)")
}

// TestOAuthHTTPClientWithHeaders_NoHeadersReusesBaseClient verifies that with
// no configured headers the helper returns the shared SSRF-safe OAuth client
// unchanged — no wrapping, and the package singleton is never mutated.
func TestOAuthHTTPClientWithHeaders_NoHeadersReusesBaseClient(t *testing.T) {
	t.Parallel()

	got := oauthHTTPClientWithHeaders("https://mcp.example.com/mcp", nil, false, nil)
	assert.Same(t, oauthHTTPClientForAllowPrivateIPs(false), got,
		"with no headers the helper must return the base OAuth client unchanged")
}

// TestInitialize_SurfacesServerErrorInReturnedError verifies that when an
// MCP server rejects the initialize call with a 4xx carrying a JSON-RPC
// error body, the error returned by Initialize contains the server's own
// explanation — not just the generic "Bad Request" from http.StatusText.
//
// Regression test for: Slack's MCP endpoint answering
//
//	400 Bad Request
//	{"jsonrpc":"2.0","id":null,"error":{"code":-32600,
//	 "message":"App is not enabled for Slack MCP server access. ..."}}
//
// where the bubbled-up error previously read only "...: Bad Request" and
// the user had no way to learn what was actually wrong.
func TestInitialize_SurfacesServerErrorInReturnedError(t *testing.T) {
	t.Parallel()

	const msg = "App is not enabled for Slack MCP server access."

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":%q}}`, msg)
	}))
	defer server.Close()

	// Pre-populate a token so the transport doesn't try to trigger OAuth on
	// the 401 path — we want to exercise the "server rejected us with a
	// non-auth failure" code path.
	store := NewInMemoryTokenStore()
	require.NoError(t, store.StoreToken(server.URL, &OAuthToken{AccessToken: "at", TokenType: "Bearer"}))

	client := newRemoteClient(server.URL, "streamable", nil, store, nil, false, nil)

	_, err := client.Initialize(t.Context(), nil)
	require.Error(t, err, "Initialize should fail against a server that rejects initialize")
	assert.Contains(t, err.Error(), msg,
		"Initialize error must surface the server's JSON-RPC error message (%q), got: %v", msg, err)
	assert.Contains(t, err.Error(), "400",
		"Initialize error should include the HTTP status code so the user knows it was a server rejection, got: %v", err)
}

// TestInitialize_NonInteractiveCtxDefersOAuthAndDoesNotBlock verifies that
// when Initialize runs against a server that requires OAuth (responds with
// 401 + WWW-Authenticate) under a context flagged with
// WithoutInteractivePrompts, the call:
//
//   - returns promptly,
//   - returns an error that satisfies IsAuthorizationRequired,
//   - never opens a callback HTTP server (i.e. doesn't try to bind a port).
//
// Regression test for: "docker agent run ./examples/slack.yaml" hanging
// during startup. The TUI was not yet ready to render the OAuth dialog,
// the elicitation goroutine was blocked on a synchronous channel send,
// and Ctrl-C couldn't reach it.
func TestInitialize_NonInteractiveCtxDefersOAuthAndDoesNotBlock(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource="https://example.test/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := newRemoteClient(server.URL, "streamable", nil, NewInMemoryTokenStore(), nil, false, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	nonInteractiveCtx := WithoutInteractivePrompts(ctx)

	done := make(chan error, 1)
	go func() {
		_, err := client.Initialize(nonInteractiveCtx, nil)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "Initialize should fail with a deferred-auth error in non-interactive ctx")
		assert.True(t, IsAuthorizationRequired(err),
			"non-interactive Initialize should return IsAuthorizationRequired, got: %v", err)
	case <-ctx.Done():
		t.Fatalf("Initialize blocked for too long; non-interactive ctx must short-circuit OAuth: %v", ctx.Err())
	}
}

// TestInitialize_OAuthDefersWhenElicitationBridgeNotReady verifies that
// when Initialize runs against a server that requires OAuth under a
// regular interactive context but no elicitation handler has been wired
// up yet (the runtime's configureToolsetHandlers hasn't run for this
// toolset), Initialize returns the same recognisable
// AuthorizationRequiredError as the explicit non-interactive deferral
// path — not an opaque "OAuth flow failed: ... no elicitation handler
// configured" message.
//
// Pairs with TestInitialize_NonInteractiveCtxDefersOAuthAndDoesNotBlock:
// that test exercises the explicit deferral via the
// WithoutInteractivePrompts marker; this one exercises the safety net
// for when the marker is missing (e.g. an early MCP probe issued from a
// code path that hasn't been taught about the marker yet) but the
// runtime hasn't attached its elicitation handler. In that situation
// the toolset must be quietly retried on the next conversation turn,
// when configureToolsetHandlers has wired everything up; surfacing a
// raw "no elicitation handler configured" error to the user
// communicates a confusing internal-state problem instead.
func TestInitialize_OAuthDefersWhenElicitationBridgeNotReady(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		// 401 + WWW-Authenticate to drive the OAuth transport into the
		// elicitation step. The resource URL points back at our own server
		// so the metadata fetches don't blow up on DNS — we want the test
		// to actually reach the elicitation call so the no-handler branch
		// is exercised.
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource=%q`, srv.URL+"/.well-known/oauth-protected-resource"))
		w.WriteHeader(http.StatusUnauthorized)
	})
	// 404 on every well-known endpoint so the OAuth flow falls through
	// to default metadata (no registration endpoint, no scopes) and gets
	// to the elicitation step quickly.
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	// Default newRemoteClient: managed=false, so the unmanaged OAuth
	// flow runs. That path reaches requestElicitation without needing
	// dynamic client registration, which keeps the test focused on the
	// bridge-not-ready behaviour.
	client := newRemoteClient(srv.URL, "streamable", nil, NewInMemoryTokenStore(), nil, false, nil)

	// Plain interactive ctx (no WithoutInteractivePrompts marker). The
	// elicitation handler is intentionally not wired up.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.Initialize(ctx, nil)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "Initialize should fail with a deferred-auth error when no elicitation handler is wired up")
		assert.True(t, IsAuthorizationRequired(err),
			"Initialize must return AuthorizationRequiredError when the runtime hasn't attached an elicitation handler yet (so the toolset is silently retried on the next conversation turn instead of surfacing a confusing 'no elicitation handler configured' message); got: %v", err)
	case <-ctx.Done():
		t.Fatalf("Initialize blocked for too long: %v", ctx.Err())
	}
}

// TestCreateHTTPClient_PersistsCookies verifies that the *http.Client returned
// by createHTTPClient has a cookie jar, so sticky-session cookies set by the
// remote MCP ingress are echoed back on subsequent requests.
func TestCreateHTTPClient_PersistsCookies(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)
		switch n {
		case 1:
			if _, err := r.Cookie("mcp_session"); err == nil {
				t.Errorf("first request must not carry mcp_session cookie, got one")
			}
			w.Header().Set("Set-Cookie", "mcp_session=abc123; Path=/")
			w.WriteHeader(http.StatusOK)
		default:
			cookie := r.Header.Get("Cookie")
			if !strings.Contains(cookie, "mcp_session=abc123") {
				t.Errorf("subsequent request must carry mcp_session=abc123, got Cookie=%q", cookie)
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client := newRemoteClient(server.URL, "streamable", nil, NewInMemoryTokenStore(), nil, false, nil)
	httpClient, _, err := client.createHTTPClient()
	require.NoError(t, err)
	require.NotNil(t, httpClient.Jar, "createHTTPClient must attach a cookie jar so sticky sessions stick")

	req1, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp1, err := httpClient.Do(req1)
	require.NoError(t, err)
	_ = resp1.Body.Close()

	req2, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp2, err := httpClient.Do(req2)
	require.NoError(t, err)
	_ = resp2.Body.Close()

	require.Equal(t, int32(2), requestCount.Load(), "handler should have served both requests")
}

func TestNewRemoteToolsetWithAllowPrivateIPsPropagatesToClient(t *testing.T) {
	t.Parallel()

	ts := NewRemoteToolsetWithAllowPrivateIPs("internal", "https://mcp.example.com/mcp", "streamable", nil, nil, true)
	client, ok := ts.mcpClient.(*remoteMCPClient)
	require.True(t, ok, "remote toolset should use remoteMCPClient")
	require.True(t, client.allowPrivateIPs, "allow_private_ips should be stored on remote client")
}

// shortTempDir returns a temp dir with a short path so unix socket paths
// created under it stay within the platform limit (macOS caps sun_path at
// ~104 bytes, which t.TempDir()'s long, test-name-derived paths can exceed).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rc") //nolint:forbidigo,usetesting // need a short path for the unix sun_path limit (~104 bytes); t.TempDir() embeds the long test name and overflows it
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestRemoteClientUnixSocket verifies the remote client can connect to an MCP
// server listening on a unix socket via a unix:// URL.
func TestRemoteClientUnixSocket(t *testing.T) {
	t.Parallel()

	server := gomcp.NewServer(&gomcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "ping", Description: "ping"}, func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, struct{}, error) {
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "pong"}}}, struct{}{}, nil
	})

	sockPath := filepath.Join(shortTempDir(t), "mcp.sock")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sockPath)
	require.NoError(t, err)
	httpServer := &http.Server{Handler: gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return server }, nil)}
	go func() { _ = httpServer.Serve(ln) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	client := newRemoteClient("unix://"+sockPath, "streamable", nil, NewInMemoryTokenStore(), nil, false, nil)
	_, err = client.Initialize(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.WithoutCancel(t.Context())) })

	var names []string
	for tool, err := range client.ListTools(t.Context(), nil) {
		require.NoError(t, err)
		names = append(names, tool.Name)
	}
	assert.Equal(t, []string{"ping"}, names)
}

// TestRemoteClientCallToolAbortsOnContextDeadline verifies the transport-level
// assumption the call_timeout design relies on: canceling the context passed
// to a real remoteMCPClient.CallTool (streamable transport) aborts the
// client's POST promptly, and the server observes that abort as its own
// request context being canceled — rather than the tool call running to
// completion in the background while the client just stops waiting.
func TestRemoteClientCallToolAbortsOnContextDeadline(t *testing.T) {
	t.Parallel()

	serverSawCancel := make(chan struct{})
	server := gomcp.NewServer(&gomcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "hang", Description: "hangs until canceled"},
		func(ctx context.Context, _ *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, struct{}, error) {
			<-ctx.Done()
			close(serverSawCancel)
			return nil, struct{}{}, ctx.Err()
		})

	httpServer := httptest.NewServer(gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return server }, nil))
	defer httpServer.Close()

	client := newRemoteClient(httpServer.URL, "streamable", nil, NewInMemoryTokenStore(), nil, false, nil)
	_, err := client.Initialize(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = client.Close(context.WithoutCancel(t.Context())) }()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = client.CallTool(ctx, &gomcp.CallToolParams{Name: "hang"})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second, "CallTool must abort promptly at the context deadline, not hang until the server responds")

	select {
	case <-serverSawCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("server-side tool handler never observed context cancellation; the client's POST was not aborted at the deadline")
	}
}

// mutableEnvProvider is a context-aware, mutable environment.Provider for
// tests that need to change a value between two requests on the same
// connection.
type mutableEnvProvider struct {
	mu   sync.Mutex
	vals map[string]string
}

func (p *mutableEnvProvider) Get(_ context.Context, name string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.vals[name]
	return v, ok
}

func (p *mutableEnvProvider) set(name, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.vals[name] = value
}

// headerCaptureMCPServer runs a real streamable MCP server behind a
// middleware that records the headers of every request it serves.
type headerCaptureMCPServer struct {
	*httptest.Server

	mu       sync.Mutex
	captured []http.Header
}

func newHeaderCaptureMCPServer(t *testing.T) *headerCaptureMCPServer {
	t.Helper()

	server := gomcp.NewServer(&gomcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "ping", Description: "ping"}, func(context.Context, *gomcp.CallToolRequest, struct{}) (*gomcp.CallToolResult, struct{}, error) {
		return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "pong"}}}, struct{}{}, nil
	})

	srv := &headerCaptureMCPServer{}
	inner := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return server }, nil)
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.mu.Lock()
		srv.captured = append(srv.captured, r.Header.Clone())
		srv.mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// snapshot returns the number of requests captured so far.
func (s *headerCaptureMCPServer) snapshot() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.captured)
}

// valuesSince returns the given header's value for every request captured
// after the from snapshot.
func (s *headerCaptureMCPServer) valuesSince(from int, header string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, h := range s.captured[from:] {
		out = append(out, h.Get(header))
	}
	return out
}

// TestRemoteClient_ExpandsEnvHeadersPerRequest is the stale-token regression
// test: a configured header referencing ${env.X} must be re-expanded on every
// HTTP request of a live connection (same long-lived http.Client/transport),
// so a value rotated in the environment provider between two requests is
// picked up without a reconnect.
func TestRemoteClient_ExpandsEnvHeadersPerRequest(t *testing.T) {
	t.Parallel()

	srv := newHeaderCaptureMCPServer(t)

	env := &mutableEnvProvider{vals: map[string]string{"TOKEN": "token-1"}}
	headers := map[string]string{"X-Env-Token": "${env.TOKEN}"}
	client := newRemoteClient(srv.URL, "streamable", headers, NewInMemoryTokenStore(), nil, false, env)

	_, err := client.Initialize(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.WithoutCancel(t.Context())) })

	listTools := func() {
		t.Helper()
		for _, err := range client.ListTools(t.Context(), nil) {
			require.NoError(t, err)
		}
	}

	listTools()
	before := srv.snapshot()
	require.Positive(t, before, "server should have seen the initialize + tools/list requests")
	for _, v := range srv.valuesSince(0, "X-Env-Token") {
		assert.Equal(t, "token-1", v, "every request before the rotation must carry the initial env value")
	}

	// Rotate the env-backed value; the SAME live connection must pick it up.
	env.set("TOKEN", "token-2")
	listTools()

	values := srv.valuesSince(before, "X-Env-Token")
	require.NotEmpty(t, values, "second tools/list must reach the server")
	for _, v := range values {
		assert.Equal(t, "token-2", v, "requests after the env rotation must carry the fresh value, not the stale one")
	}
}

// TestRemoteClient_ResolvesUpstreamHeaderPlaceholdersPerRequest verifies the
// pre-existing ${headers.X} contract still holds alongside env expansion:
// values are resolved per request from the upstream headers carried in the
// request context, so two calls with different contexts produce different
// outbound headers on the same connection.
func TestRemoteClient_ResolvesUpstreamHeaderPlaceholdersPerRequest(t *testing.T) {
	t.Parallel()

	srv := newHeaderCaptureMCPServer(t)

	env := &mutableEnvProvider{vals: map[string]string{"TOKEN": "env-tok"}}
	headers := map[string]string{
		"Authorization": "${headers.Authorization}",
		"X-Env-Token":   "${env.TOKEN}",
	}
	client := newRemoteClient(srv.URL, "streamable", headers, NewInMemoryTokenStore(), nil, false, env)

	_, err := client.Initialize(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.WithoutCancel(t.Context())) })

	listTools := func(ctx context.Context) {
		t.Helper()
		for _, err := range client.ListTools(ctx, nil) {
			require.NoError(t, err)
		}
	}

	up1 := http.Header{}
	up1.Set("Authorization", "Bearer upstream-1")
	before := srv.snapshot()
	listTools(upstream.WithHeaders(t.Context(), up1))
	for _, v := range srv.valuesSince(before, "Authorization") {
		assert.Equal(t, "Bearer upstream-1", v, "first call must carry the first upstream Authorization")
	}

	up2 := http.Header{}
	up2.Set("Authorization", "Bearer upstream-2")
	before = srv.snapshot()
	listTools(upstream.WithHeaders(t.Context(), up2))
	values := srv.valuesSince(before, "Authorization")
	require.NotEmpty(t, values)
	for _, v := range values {
		assert.Equal(t, "Bearer upstream-2", v, "second call must carry the second upstream Authorization")
	}
	// Env expansion applies on the same requests too.
	for _, v := range srv.valuesSince(before, "X-Env-Token") {
		assert.Equal(t, "env-tok", v)
	}
}

// TestRemoteClient_ExpandsMixedEnvAndUpstreamPlaceholdersInOneValue covers a
// single configured value mixing both placeholder kinds: phase one resolves
// ${env.X} and leaves ${headers.Y} untouched, phase two fills it from the
// upstream headers in the request context. Both phases run per request on
// the same connection, so rotating either source shows up on the next call.
func TestRemoteClient_ExpandsMixedEnvAndUpstreamPlaceholdersInOneValue(t *testing.T) {
	t.Parallel()

	srv := newHeaderCaptureMCPServer(t)

	env := &mutableEnvProvider{vals: map[string]string{"SCHEME": "Env"}}
	headers := map[string]string{"X-Combined": "${env.SCHEME} ${headers.X-Upstream-Token}"}
	client := newRemoteClient(srv.URL, "streamable", headers, NewInMemoryTokenStore(), nil, false, env)

	_, err := client.Initialize(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.WithoutCancel(t.Context())) })

	listTools := func(ctx context.Context) {
		t.Helper()
		for _, err := range client.ListTools(ctx, nil) {
			require.NoError(t, err)
		}
	}

	up1 := http.Header{}
	up1.Set("X-Upstream-Token", "upstream-1")
	before := srv.snapshot()
	listTools(upstream.WithHeaders(t.Context(), up1))
	values := srv.valuesSince(before, "X-Combined")
	require.NotEmpty(t, values)
	for _, v := range values {
		assert.Equal(t, "Env upstream-1", v,
			"both placeholder kinds in one value must resolve to the combined outbound value")
	}

	// Rotate both sources; the SAME live connection must combine the fresh
	// values on the next request.
	env.set("SCHEME", "Env2")
	up2 := http.Header{}
	up2.Set("X-Upstream-Token", "upstream-2")
	before = srv.snapshot()
	listTools(upstream.WithHeaders(t.Context(), up2))
	values = srv.valuesSince(before, "X-Combined")
	require.NotEmpty(t, values)
	for _, v := range values {
		assert.Equal(t, "Env2 upstream-2", v,
			"the mixed value must be re-expanded per request from both sources")
	}
}

// TestOAuthHTTPClientWithHeaders_ResolverKeepsEnvHeadersFreshAndHostScoped
// verifies the OAuth channel gets the same per-request dynamic expansion as
// the main channel — a rotated ${env.X} value reaches the next same-host
// OAuth request — while custom headers still never leak to a third-party
// authorization-server host.
func TestOAuthHTTPClientWithHeaders_ResolverKeepsEnvHeadersFreshAndHostScoped(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var mcpHostValues []string
	mcpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mcpHostValues = append(mcpHostValues, r.Header.Get("X-Env-Header"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer mcpServer.Close()

	var thirdPartyHeader atomic.Value
	thirdParty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdPartyHeader.Store(r.Header.Get("X-Env-Header"))
		w.WriteHeader(http.StatusOK)
	}))
	defer thirdParty.Close()

	env := &mutableEnvProvider{vals: map[string]string{"SECRET": "secret-1"}}
	headers := map[string]string{"X-Env-Header": "${env.SECRET}"}
	// Same wiring as remote.go createHTTPClient: the remote client's
	// expandHeaders is the per-request resolver for the OAuth channel.
	mcpClient := newRemoteClient(mcpServer.URL, "streamable", headers, NewInMemoryTokenStore(), nil, true, env)
	client := oauthHTTPClientWithHeaders(mcpServer.URL, headers, true, mcpClient.expandHeaders)

	get := func(url string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
	}

	get(mcpServer.URL)
	env.set("SECRET", "secret-2")
	get(mcpServer.URL)

	mu.Lock()
	assert.Equal(t, []string{"secret-1", "secret-2"}, mcpHostValues,
		"same-host OAuth requests must re-expand env-backed headers per request")
	mu.Unlock()

	get(thirdParty.URL)
	v, _ := thirdPartyHeader.Load().(string)
	assert.Empty(t, v,
		"requests to a third-party host must NOT carry the configured header even with a resolver (credential-leak guard)")
}
