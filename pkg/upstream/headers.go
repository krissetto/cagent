// Package upstream provides utilities for propagating HTTP headers
// from incoming API requests to outbound toolset HTTP calls.
package upstream

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/js"
)

type contextKey struct{}

// WithHeaders returns a new context carrying the given HTTP headers.
func WithHeaders(ctx context.Context, h http.Header) context.Context {
	return context.WithValue(ctx, contextKey{}, h)
}

// HeadersFromContext retrieves upstream HTTP headers from the context.
// Returns nil if no headers are present.
func HeadersFromContext(ctx context.Context) http.Header {
	h, _ := ctx.Value(contextKey{}).(http.Header)
	return h
}

// Handler wraps an http.Handler to store the incoming HTTP request
// headers in the request context for downstream toolset forwarding.
func Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithHeaders(r.Context(), r.Header.Clone())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// HeaderResolver resolves configured header values into the final values to
// set on an outbound request. It is invoked once per request with the
// request's context, so implementations can consult dynamic sources (e.g.
// upstream headers stored in the context, or environment providers backed
// by rotating credentials) without going stale on long-lived connections.
type HeaderResolver func(ctx context.Context, headers map[string]string) map[string]string

// NewHeaderTransport wraps an http.RoundTripper to set custom headers. The
// first request pins the origin; redirects to another origin do not receive
// the configured headers.
func NewHeaderTransport(base http.RoundTripper, headers map[string]string) http.RoundTripper {
	return newHeaderTransport(base, "", headers, nil)
}

// NewHeaderTransportForOrigin is like NewHeaderTransport, but pins the origin
// before the first request so a shared transport cannot be initialized by the
// wrong caller.
func NewHeaderTransportForOrigin(base http.RoundTripper, origin string, headers map[string]string) http.RoundTripper {
	return newHeaderTransport(base, origin, headers, nil)
}

// NewHeaderTransportWithResolver is like NewHeaderTransport but resolves the
// header values through resolve on every request.
func NewHeaderTransportWithResolver(base http.RoundTripper, headers map[string]string, resolve HeaderResolver) http.RoundTripper {
	return newHeaderTransport(base, "", headers, resolve)
}

// NewHeaderTransportWithResolverForOrigin combines per-request resolution with
// an explicitly pinned origin.
func NewHeaderTransportWithResolverForOrigin(base http.RoundTripper, origin string, headers map[string]string, resolve HeaderResolver) http.RoundTripper {
	return newHeaderTransport(base, origin, headers, resolve)
}

func newHeaderTransport(base http.RoundTripper, origin string, headers map[string]string, resolve HeaderResolver) http.RoundTripper {
	if resolve == nil {
		resolve = ResolveHeaders
	}
	return &headerTransport{base: base, origin: parseOrigin(origin), headers: headers, resolve: resolve}
}

type headerTransport struct {
	base       http.RoundTripper
	originOnce sync.Once
	origin     string
	headers    map[string]string
	resolve    HeaderResolver
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Response != nil && req.Response.Request.URL.Scheme == "https" && req.URL.Scheme != "https" {
		return nil, fmt.Errorf("refusing HTTPS downgrade redirect to %q", req.URL.Redacted())
	}

	t.originOnce.Do(func() {
		if t.origin == "" {
			t.origin = requestOrigin(req)
		}
	})

	req = req.Clone(req.Context())
	if requestOrigin(req) == t.origin {
		for key, value := range t.resolve(req.Context(), t.headers) {
			req.Header.Set(key, value)
		}
	}
	return t.base.RoundTrip(req)
}

// SameOrigin reports whether target has the same scheme, host, and effective
// port as rawURL.
func SameOrigin(rawURL string, target *url.URL) bool {
	return parseOrigin(rawURL) != "" && parseOrigin(rawURL) == origin(target)
}

func parseOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return origin(u)
}

func requestOrigin(req *http.Request) string {
	if req == nil {
		return ""
	}
	return origin(req.URL)
}

func origin(u *url.URL) string {
	if u == nil || u.Scheme == "" || u.Host == "" {
		return ""
	}

	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.Unmap().String()
	}

	port := u.Port()
	if port != "" && (scheme != "http" || port != "80") && (scheme != "https" || port != "443") {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

// ResolveHeaders resolves ${headers.NAME} placeholders in header values
// using upstream headers from the context. Header names in the placeholder
// are case-insensitive, matching HTTP header convention.
//
// For example, given the config header:
//
//	Authorization: ${headers.Authorization}
//
// and an upstream request with "Authorization: Bearer token", the resolved
// value will be "Bearer token".
func ResolveHeaders(ctx context.Context, headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return headers
	}

	upstream := HeadersFromContext(ctx)
	if upstream == nil {
		return headers
	}

	return js.ExpandMapFunc(headers, "headers", upstream.Get, rewriteBracketNotation)
}

// headerPlaceholderRe matches ${headers.NAME} and captures the header
// name so we can rewrite it to bracket notation for the JS runtime.
var headerPlaceholderRe = regexp.MustCompile(`\$\{\s*headers\.([^}]+)\}`)

// rewriteBracketNotation rewrites ${headers.NAME} to ${headers["NAME"]}
// so that header names containing hyphens (e.g. X-Request-Id) are
// accessed correctly by the JS runtime.
func rewriteBracketNotation(text string) string {
	return headerPlaceholderRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := headerPlaceholderRe.FindStringSubmatch(m)
		name := strings.TrimSpace(parts[1])
		return `${headers["` + name + `"]}`
	})
}
