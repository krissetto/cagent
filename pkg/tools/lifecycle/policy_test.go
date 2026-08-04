package lifecycle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestPolicyFromConfig_NilUsesResilientDefaults(t *testing.T) {
	t.Parallel()
	p := PolicyFromConfig("test", nil)
	assert.Equal(t, RestartOnFailure, p.Restart)
	assert.Equal(t, 5, p.MaxAttempts)
	assert.NotNil(t, p.Logger)
}

func TestPolicyFromConfig_StrictProfile(t *testing.T) {
	t.Parallel()
	p := PolicyFromConfig("test", &latest.LifecycleConfig{
		Profile: latest.LifecycleProfileStrict,
	})
	assert.Equal(t, RestartNever, p.Restart)
	assert.Equal(t, -1, p.MaxAttempts)
}

func TestPolicyFromConfig_BestEffortProfile(t *testing.T) {
	t.Parallel()
	p := PolicyFromConfig("test", &latest.LifecycleConfig{
		Profile: latest.LifecycleProfileBestEffort,
	})
	assert.Equal(t, RestartNever, p.Restart)
}

func TestPolicyFromConfig_ExplicitOverrides(t *testing.T) {
	t.Parallel()
	cfg := &latest.LifecycleConfig{
		Profile:     latest.LifecycleProfileResilient,
		Restart:     "always",
		MaxRestarts: 12,
		Backoff: &latest.BackoffConfig{
			Initial:    latest.Duration{Duration: 500 * time.Millisecond},
			Max:        latest.Duration{Duration: 10 * time.Second},
			Multiplier: 1.5,
			Jitter:     0.3,
		},
	}
	p := PolicyFromConfig("test", cfg)
	assert.Equal(t, RestartAlways, p.Restart)
	assert.Equal(t, 12, p.MaxAttempts)
	assert.Equal(t, 500*time.Millisecond, p.Backoff.Initial)
	assert.Equal(t, 10*time.Second, p.Backoff.Max)
	assert.InDelta(t, 1.5, p.Backoff.Multiplier, 0.001)
	assert.InDelta(t, 0.3, p.Backoff.Jitter, 0.001)
}

func TestPolicyFromConfig_PartialOverridesKeepProfileDefaults(t *testing.T) {
	t.Parallel()
	cfg := &latest.LifecycleConfig{
		Profile:     latest.LifecycleProfileResilient,
		MaxRestarts: 7,
	}
	p := PolicyFromConfig("test", cfg)
	assert.Equal(t, RestartOnFailure, p.Restart, "profile default preserved")
	assert.Equal(t, 7, p.MaxAttempts, "explicit override applied")
}

func TestPolicyFromConfig_StartupTimeout(t *testing.T) {
	t.Parallel()

	// Nil config and non-strict profiles default to no timeout.
	assert.Equal(t, time.Duration(0), PolicyFromConfig("test", nil).StartupTimeout)
	assert.Equal(t, time.Duration(0), PolicyFromConfig("test", &latest.LifecycleConfig{
		Profile: latest.LifecycleProfileResilient,
	}).StartupTimeout)

	// The strict profile carries a 30s default.
	assert.Equal(t, 30*time.Second, PolicyFromConfig("test", &latest.LifecycleConfig{
		Profile: latest.LifecycleProfileStrict,
	}).StartupTimeout)

	// An explicit value wins over the profile default.
	assert.Equal(t, 5*time.Second, PolicyFromConfig("test", &latest.LifecycleConfig{
		StartupTimeout: latest.Duration{Duration: 5 * time.Second},
	}).StartupTimeout)
}

func TestPolicyFromConfig_CallTimeout(t *testing.T) {
	t.Parallel()

	// Nil config and an unset field both mean "no timeout" — call_timeout
	// has no profile default, unlike startup_timeout.
	assert.Equal(t, time.Duration(0), PolicyFromConfig("test", nil).CallTimeout)
	assert.Equal(t, time.Duration(0), PolicyFromConfig("test", &latest.LifecycleConfig{
		Profile: latest.LifecycleProfileStrict,
	}).CallTimeout, "call_timeout has no profile default, even under strict")

	assert.Equal(t, 45*time.Second, PolicyFromConfig("test", &latest.LifecycleConfig{
		CallTimeout: latest.Duration{Duration: 45 * time.Second},
	}).CallTimeout)
}

// TestPolicyFromConfig_NilEquivalentToZeroPolicy pins the equivalence that
// NewGatewayToolset's lifecycle wiring relies on: callers that previously
// passed no policy (zero value) must see the same supervisor behavior as
// PolicyFromConfig(name, nil), so wiring lifecycle config through the
// gateway path is not a behavior change for toolsets without a lifecycle
// block. Logger is the only permitted difference.
func TestPolicyFromConfig_NilEquivalentToZeroPolicy(t *testing.T) {
	t.Parallel()

	zero := Policy{}
	fromNil := PolicyFromConfig("test", nil)

	assert.Equal(t, zero.Restart, fromNil.Restart)
	assert.Equal(t, zero.maxAttempts(), fromNil.maxAttempts())
	assert.Equal(t, zero.Backoff, fromNil.Backoff)
	assert.Equal(t, zero.StartupTimeout, fromNil.StartupTimeout)
	assert.Equal(t, zero.CallTimeout, fromNil.CallTimeout)
	assert.Nil(t, zero.Logger)
	assert.NotNil(t, fromNil.Logger, "PolicyFromConfig always attaches a logger")
}

func TestParseRestart(t *testing.T) {
	t.Parallel()
	cases := map[string]Restart{
		"":           RestartOnFailure,
		"on_failure": RestartOnFailure,
		"never":      RestartNever,
		"always":     RestartAlways,
	}
	for in, want := range cases {
		assert.Equal(t, want, ParseRestart(in), "input=%q", in)
	}
}
