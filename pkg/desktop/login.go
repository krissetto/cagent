package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type DockerHubInfo struct {
	Username string `json:"id"`
	Email    string `json:"email,omitempty"`
}

// GetToken returns Docker Desktop's access token. Desktop's newer auth stack
// (auth v2) serves whatever its in-memory token source holds and never
// refreshes on GET, so a stuck background refresher makes it return the same
// expired JWT forever — or nothing at all when its read-time refresh failed.
// When that happens we force a refresh on Desktop's side.
func GetToken(ctx context.Context) string {
	token, err := fetchToken(ctx)
	if err == nil && token != "" && !tokenExpired(token) {
		return token
	}

	logUnusableToken(ctx, token, err)

	if token == "" && !isLoggedIn(ctx) {
		// Signed out (or Desktop unreachable): a forced refresh can't
		// produce a token, and its polling budget would delay every caller.
		return ""
	}

	if fresh := forceTokenRefresh(ctx); fresh != "" {
		slog.InfoContext(ctx, "Recovered a fresh token from Docker Desktop",
			"fingerprint", tokenFingerprint(fresh))
		return fresh
	}
	if token == "" {
		slog.WarnContext(ctx, "Token refresh failed, no token available")
		return ""
	}
	slog.WarnContext(ctx, "Token refresh failed, sending a token known to be expired",
		"fingerprint", tokenFingerprint(token),
		"expired_for", expiredFor(token))
	return token
}

// logUnusableToken records evidence of why Docker Desktop's token can't be
// used as-is, so gateway auth failures can be attributed to a stale or broken
// Desktop session rather than a rejection of a valid token.
func logUnusableToken(ctx context.Context, token string, err error) {
	switch {
	case err != nil:
		slog.WarnContext(ctx, "Failed to fetch a token from Docker Desktop", "error", err)
	case token == "":
		slog.WarnContext(ctx, "Docker Desktop served an empty token")
	default:
		attrs := []any{"fingerprint", tokenFingerprint(token), "expired_for", expiredFor(token)}
		if exp, ok := tokenExpiry(token); ok {
			attrs = append(attrs, "expires_at", exp.UTC().Format(time.RFC3339))
		}
		slog.WarnContext(ctx, "Docker Desktop served an expired token", attrs...)
	}
}

// tokenFingerprint returns a short non-reversible identifier, safe to log.
func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
}

// expiredFor returns how long ago the token's exp claim passed.
func expiredFor(token string) string {
	exp, ok := tokenExpiry(token)
	if !ok {
		return "unknown"
	}
	return time.Since(exp).Round(time.Second).String()
}

// tokenExpiry returns the token's exp claim, or false when the token doesn't
// parse or carries no exp claim.
func tokenExpiry(token string) (time.Time, bool) {
	parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return time.Time{}, false
	}
	exp, err := parsed.Claims.GetExpirationTime()
	if err != nil || exp == nil {
		return time.Time{}, false
	}
	return exp.Time, true
}

func GetUserInfo(ctx context.Context) DockerHubInfo {
	var info DockerHubInfo
	_ = ClientBackend.Get(ctx, "/registry/info", &info)
	return info
}

func fetchToken(ctx context.Context) (string, error) {
	var token string
	err := ClientBackend.Get(ctx, "/registry/token", &token)
	return token, err
}

func isLoggedIn(ctx context.Context) bool {
	var loggedIn bool
	if err := ClientBackend.Get(ctx, "/registry/is-logged-in", &loggedIn); err != nil {
		return false
	}
	return loggedIn
}

// tokenExpired reports whether the JWT's exp claim is in the past, with
// leeway for clock skew between this machine and the token issuer.
// Tokens that don't parse or carry no exp claim are treated as valid.
func tokenExpired(token string) bool {
	exp, ok := tokenExpiry(token)
	if !ok {
		return false
	}
	return exp.Before(time.Now().Add(-expiryLeeway))
}

const expiryLeeway = 30 * time.Second

var refreshState struct {
	sync.Mutex

	nextAttempt time.Time     // earliest time a new refresh may start
	inflight    chan struct{} // closed when the in-flight refresh completes
	result      string        // token produced by the last refresh, "" if none
}

// Vars, not consts, so tests can shorten them.
var (
	refreshCooldown       = 30 * time.Second
	refreshFailureBackoff = 2 * time.Minute
	refreshBudget         = 10 * time.Second
	refreshPollInterval   = 500 * time.Millisecond
)

// forceTokenRefresh nudges Docker Desktop to reload its session from the OS
// credential store and refresh it — the same hook the Docker CLI triggers
// after `docker login`. Desktop handles it asynchronously, so we poll until a
// non-expired token shows up. Returns "" if no fresh token was obtained.
//
// Refreshes are singleflighted: one goroutine talks to Desktop while
// concurrent callers wait for its result (or bail out when their own context
// is canceled). Attempts are rate-limited so a persistently stale Desktop
// doesn't stall every request.
func forceTokenRefresh(ctx context.Context) string {
	refreshState.Lock()

	if inflight := refreshState.inflight; inflight != nil {
		refreshState.Unlock()
		return awaitRefresh(ctx, inflight)
	}

	if time.Now().Before(refreshState.nextAttempt) {
		// Rate-limited, but a refresh may have just completed: reuse its
		// result if still valid.
		token := refreshState.result
		refreshState.Unlock()
		if token != "" && !tokenExpired(token) {
			return token
		}
		return ""
	}

	done := make(chan struct{})
	refreshState.inflight = done
	refreshState.Unlock()

	go func() {
		// Detached from the caller: the refresh benefits all requests, so a
		// single canceled request must not abort it. refreshBudget bounds the
		// whole attempt (POST + polling).
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshBudget)
		defer cancel()

		token := runTokenRefresh(ctx)

		refreshState.Lock()
		defer refreshState.Unlock()
		refreshState.result = token
		backoff := refreshCooldown
		if token == "" {
			backoff = refreshFailureBackoff
		}
		refreshState.nextAttempt = time.Now().Add(backoff)
		refreshState.inflight = nil
		close(done)
	}()

	return awaitRefresh(ctx, done)
}

func awaitRefresh(ctx context.Context, done <-chan struct{}) string {
	select {
	case <-done:
		refreshState.Lock()
		defer refreshState.Unlock()
		return refreshState.result
	case <-ctx.Done():
		return ""
	}
}

func runTokenRefresh(ctx context.Context) string {
	slog.WarnContext(ctx, "Forcing a Docker Desktop token refresh")
	if err := postRefreshNudge(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to trigger Docker Desktop token refresh", "error", err)
		return ""
	}

	ticker := time.NewTicker(refreshPollInterval)
	defer ticker.Stop()

	for {
		// Check right away: Desktop may have refreshed synchronously.
		if token, err := fetchToken(ctx); err == nil && token != "" && !tokenExpired(token) {
			return token
		}
		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "Docker Desktop did not deliver a fresh token in time")
			return ""
		case <-ticker.C:
		}
	}
}

// postRefreshNudge caps the POST to a slice of the refresh budget so a slow
// Desktop can't consume it all and starve the polling loop that follows.
func postRefreshNudge(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, refreshBudget/3)
	defer cancel()
	return ClientBackend.Post(ctx, "/registry/credstore-updated")
}
