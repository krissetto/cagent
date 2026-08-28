package tools

import (
	"errors"
	"math/rand/v2"
	"time"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

const (
	startBackoffBase = 15 * time.Second
	startBackoffMax  = 5 * time.Minute
)

// startBackoffRetryable reports whether err carries a retryable HTTP status
// that warrants pacing the next start attempt: 429, 408, 500, 502, 503, 504,
// or 529 (see modelerrors.isRetryableStatusCode). This is a fixed
// enumeration, not a full 5xx range — codes such as 501, 505, or the
// Cloudflare 520-527 family do NOT arm the gate. Only a *modelerrors.StatusError
// in the error chain arms the gate; plain network errors and the regex
// fallback in RetryableHTTPStatus are intentionally excluded so port numbers,
// PIDs, and chunk counters in plain error text cannot arm the gate.
//
// A StatusError wins even when context.DeadlineExceeded is also in the chain.
//
// Deliberately excluded from arming (these must never pace):
//   - lifecycle.ErrServerUnavailable: missing binary / process-not-found — fast-retry.
//   - lifecycle.ErrTransport: connection refused / no such host — fast-retry.
//   - lifecycle.ErrAuthRequired / ErrCapabilityMissing: permanent — fail promptly.
//   - lifecycle.ErrInitTimeout, ErrSessionMissing: transient, handled by the
//     supervisor's own reconnect policy without per-turn pacing.
//   - Plain error strings: excluded to avoid false positives on numeric
//     patterns in port numbers or counters.
//
// Note: lifecycle.ErrServerCrashed (a server that started then crashed) is
// NOT currently surfaced by supervisor.Start(); it flows only through the
// supervisor's internal watcher goroutine. LSP crash-loop pacing is therefore
// deferred until that sentinel is propagated through the start path.
func startBackoffRetryable(err error) bool {
	var se *modelerrors.StatusError
	if !errors.As(err, &se) {
		return false
	}
	return modelerrors.RetryableHTTPStatus(se)
}

// computeStartBackoff returns the exponential backoff delay for attempt
// (1-indexed), capped at startBackoffMax with additive 0–20% jitter.
// jitterFn overrides the default additiveJitter when non-nil.
func computeStartBackoff(attempt int, jitterFn func(time.Duration) time.Duration) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}

	// Double until cap; overflow-safe because we stop at startBackoffMax.
	nominal := startBackoffBase
	for i := 1; i < attempt; i++ {
		nominal *= 2
		if nominal >= startBackoffMax {
			nominal = startBackoffMax
			break
		}
	}

	if jitterFn != nil {
		return jitterFn(nominal)
	}
	return additiveJitter(nominal)
}

// additiveJitter returns d + rand.N([0, d/5]), giving delay ∈ [d, 1.2d].
func additiveJitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.N(int64(d/5)+1))
}

// retryAfterHint extracts the Retry-After duration from a *StatusError in the
// chain. Returns 0 if absent or if the error is not a *StatusError.
func retryAfterHint(err error) time.Duration {
	var se *modelerrors.StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}
