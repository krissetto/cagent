package httpclient

import (
	"context"
	"strings"
	"net"
	"net/http"
	"os"

	desktoptransport "github.com/docker/docker-agent/pkg/desktop/transport"
)

const disableDesktopProxyEnv = "CAGENT_DISABLE_DESKTOP_PROXY"

type desktopAwareTransport struct {
	direct              *http.Transport
	guarded             bool
	resolver            func(context.Context, string) ([]net.IP, error)
	newDesktopTransport func(context.Context, http.RoundTripper) http.RoundTripper

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
		newDesktopTransport: desktoptransport.NewWithDirectTransport,
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
	// Guarded transports restrict Desktop PAC routing to Docker-owned hosts;
	// all other hosts use the direct SSRF-guarded transport regardless of Desktop state.
	if desktopProxyDisabled() || isLoopbackHost(host) || (t.guarded && (!isDockerHost(host) || !t.proxySafe(req.Context(), host))) {
		return t.direct.RoundTrip(req)
	}
	desktopRT := t.newDesktopTransport(req.Context(), t.direct)
	if t.disableCompression {
		if disabler, ok := desktopRT.(interface{ DisableCompression() }); ok {
			disabler.DisableCompression()
		}
	}
	return desktopRT.RoundTrip(req)
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
	return os.Getenv(disableDesktopProxyEnv) == "1"
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
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (t *desktopAwareTransport) DisableCompression() {
	t.disableCompression = true
	t.direct.DisableCompression = true
}
