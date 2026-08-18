package httpclient

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	desktoptransport "github.com/docker/docker-agent/pkg/desktop/transport"
)

const disableDesktopProxyEnv = "DOCKER_AGENT_DISABLE_DESKTOP_PROXY"

type desktopAwareTransport struct {
	direct              *http.Transport
	guarded             bool
	resolver            func(context.Context, string) ([]net.IP, error)
	newDesktopTransport func(context.Context, http.RoundTripper) http.RoundTripper

	mu                 sync.Mutex
	desktopOnce        sync.Once
	desktopTransport   http.RoundTripper
	disableCompression bool
}

func newDesktopAwareTransport(guarded bool) http.RoundTripper {
	var direct *http.Transport
	if guarded {
		direct = NewSSRFSafeTransport()
	} else {
		direct = cloneDefaultTransport()
	}
	return &desktopAwareTransport{
		direct:  direct,
		guarded: guarded,
		resolver: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		newDesktopTransport: func(_ context.Context, direct http.RoundTripper) http.RoundTripper {
			return desktoptransport.NewDesktopTransport(direct)
		},
	}
}

// NewDesktopAwareSSRFSafeTransport returns a guarded transport that routes
// Docker-owned hostnames (docker.com, docker.io families) through Docker
// Desktop's PAC proxy when Desktop is available. All other hosts always use
// the direct SSRF-guarded transport (dial-time enforcement, defeats DNS
// rebinding). Docker Desktop is optional: absent, disabled, or when the target
// host is outside the allowlist, requests fall back to direct with the same
// SSRF protection as NewSSRFSafeTransport.
func NewDesktopAwareSSRFSafeTransport() http.RoundTripper {
	return newDesktopAwareTransport(true)
}

func newAllowPrivateIPsTransport() http.RoundTripper {
	return newDesktopAwareTransport(false)
}

func cloneDefaultTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

func (t *desktopAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if desktopProxyDisabled() || isLoopbackHost(host) {
		return t.direct.RoundTrip(req)
	}
	// Guarded transports restrict Desktop PAC routing to Docker-owned hosts;
	// all other hosts use the direct SSRF-guarded transport regardless of Desktop state.
	if t.guarded && !isDockerHost(host) {
		return t.direct.RoundTrip(req)
	}

	transport := t.selectedTransportFor(req.Context())
	if transport == t.direct || (t.guarded && !t.proxySafe(req.Context(), host)) {
		return t.direct.RoundTrip(req)
	}
	return transport.RoundTrip(req)
}

func (t *desktopAwareTransport) selectedTransportFor(ctx context.Context) http.RoundTripper {
	running, err := desktoptransport.DesktopRunning(ctx)
	if err != nil || !running {
		return t.direct
	}
	t.desktopOnce.Do(func() {
		t.desktopTransport = t.newDesktopTransport(ctx, t.direct)
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.disableCompression {
			if disabler, ok := t.desktopTransport.(interface{ DisableCompression() }); ok {
				disabler.DisableCompression()
			}
		}
	})
	return t.desktopTransport
}

func (t *desktopAwareTransport) proxySafe(ctx context.Context, host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return IsPublicIP(ip)
	}
	ips, err := t.resolver(ctx, host)
	if err != nil {
		// Fail closed: Docker-owned hostnames resolve publicly; NXDOMAIN
		// suggests a broken resolver rather than a PAC-only network.
		return false
	}
	if len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !IsPublicIP(ip) {
			return false
		}
	}
	return true
}

func desktopProxyDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(disableDesktopProxyEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}


// dockerBaseDomains lists Docker's infrastructure domains that guarded
// transports may route through Desktop's PAC proxy. Any hostname that is
// exactly one of these domains, or a subdomain of one, is eligible for
// Desktop routing; all others use the direct SSRF-guarded path.
var dockerBaseDomains = [...]string{
	"docker.com", // Hub, auth, API, desktop configuration
	"docker.io",  // Container registry: registry-1, auth, index, cdn
}

// isDockerHost reports whether hostname belongs to Docker's infrastructure
// (docker.com or docker.io, including all subdomains). Only guarded transports
// use this allowlist; unguarded transports (allow_private_ips) skip it.
func isDockerHost(hostname string) bool {
	h := strings.ToLower(strings.TrimSuffix(hostname, "."))
	for _, base := range dockerBaseDomains {
		if h == base || strings.HasSuffix(h, "."+base) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (t *desktopAwareTransport) DisableCompression() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.disableCompression = true
	t.direct.DisableCompression = true
	if disabler, ok := t.desktopTransport.(interface{ DisableCompression() }); ok {
		disabler.DisableCompression()
	}
}
