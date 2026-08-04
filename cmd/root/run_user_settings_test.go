package root

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestRunExecFlagsApplyUserSettingsLean(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		lean        bool
		leanChanged bool
		wantLean    bool
	}{
		{
			name:     "applies lean default",
			wantLean: true,
		},
		{
			name:        "keeps explicit lean false",
			leanChanged: true,
			wantLean:    false,
		},
		{
			name:        "keeps explicit lean true",
			lean:        true,
			leanChanged: true,
			wantLean:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			flags := &runExecFlags{
				lean:        tt.lean,
				leanChanged: tt.leanChanged,
			}
			flags.applyUserSettings(t.Context(), &userconfig.Settings{Lean: true})

			assert.Equal(t, tt.wantLean, flags.lean)
		})
	}
}

func TestRunExecFlagsApplyUserSettingsSafety(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		settings        userconfig.Settings
		flags           *runExecFlags
		wantDefault     session.SafetyPolicy
		wantAutoApprove bool
	}{
		{
			name:        "settings safety becomes the default",
			settings:    userconfig.Settings{Safety: latest.SafetyModeBalanced},
			wantDefault: session.SafetyPolicyBalanced,
		},
		{
			name:            "legacy YOLO maps to autonomous",
			settings:        userconfig.Settings{YOLO: true},
			wantDefault:     session.SafetyPolicyAutonomous,
			wantAutoApprove: true,
		},
		{
			name:            "safety wins over legacy YOLO at the same scope",
			settings:        userconfig.Settings{YOLO: true, Safety: latest.SafetyModeStrict},
			wantDefault:     session.SafetyPolicyStrict,
			wantAutoApprove: true,
		},
		{
			name:     "explicit --yolo=false suppresses the legacy YOLO flag",
			settings: userconfig.Settings{YOLO: true},
			flags:    &runExecFlags{yoloChanged: true},
			// Both the auto-approve mutation and the yolo-derived safety
			// default are suppressed by the explicit flag; a typed
			// settings.safety would still apply.
			wantDefault:     "",
			wantAutoApprove: false,
		},
		{
			name:            "explicit --yolo=false keeps a typed safety default",
			settings:        userconfig.Settings{YOLO: true, Safety: latest.SafetyModeBalanced},
			flags:           &runExecFlags{yoloChanged: true},
			wantDefault:     session.SafetyPolicyBalanced,
			wantAutoApprove: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			flags := tt.flags
			if flags == nil {
				flags = &runExecFlags{}
			}
			flags.applyUserSettings(t.Context(), &tt.settings)

			assert.Equal(t, tt.wantDefault, flags.defaultSafety)
			assert.Equal(t, tt.wantAutoApprove, flags.autoApprove)
		})
	}
}

// Alias options are applied after user settings and outrank them; within
// the alias, safety wins over the legacy yolo flag.
func TestRunExecFlagsApplyAliasOptionsSafety(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		alias       userconfig.Alias
		flags       *runExecFlags
		wantDefault session.SafetyPolicy
	}{
		{
			name:        "alias safety overrides the settings default",
			alias:       userconfig.Alias{Path: "x", Safety: latest.SafetyModeStrict},
			flags:       &runExecFlags{defaultSafety: session.SafetyPolicyAutonomous},
			wantDefault: session.SafetyPolicyStrict,
		},
		{
			name:        "alias legacy yolo maps to autonomous",
			alias:       userconfig.Alias{Path: "x", Yolo: true},
			flags:       &runExecFlags{defaultSafety: session.SafetyPolicyBalanced},
			wantDefault: session.SafetyPolicyAutonomous,
		},
		{
			name:        "alias safety wins over alias yolo",
			alias:       userconfig.Alias{Path: "x", Yolo: true, Safety: latest.SafetyModeBalanced},
			wantDefault: session.SafetyPolicyBalanced,
		},
		{
			name:        "alias without safety keeps the settings default",
			alias:       userconfig.Alias{Path: "x", HideToolResults: true},
			flags:       &runExecFlags{defaultSafety: session.SafetyPolicyBalanced},
			wantDefault: session.SafetyPolicyBalanced,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			flags := tt.flags
			if flags == nil {
				flags = &runExecFlags{}
			}
			flags.applyAliasOptions(t.Context(), &tt.alias)

			assert.Equal(t, tt.wantDefault, flags.defaultSafety)
		})
	}
}

// userSafetyPolicy resolves the user-owned mode: explicit CLI flags first
// (--safety over --yolo), then the alias/settings default.
func TestRunExecFlagsUserSafetyPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		flags        *runExecFlags
		want         session.SafetyPolicy
		wantExplicit bool
	}{
		{
			name: "nothing set resolves to empty",
		},
		{
			name:         "explicit --safety wins over everything",
			flags:        &runExecFlags{safety: "strict", safetyChanged: true, autoApprove: true, yoloChanged: true, defaultSafety: session.SafetyPolicyAutonomous},
			want:         session.SafetyPolicyStrict,
			wantExplicit: true,
		},
		{
			name:         "explicit --yolo wins over defaults",
			flags:        &runExecFlags{autoApprove: true, yoloChanged: true, defaultSafety: session.SafetyPolicyBalanced},
			want:         session.SafetyPolicyAutonomous,
			wantExplicit: true,
		},
		{
			name:  "defaults apply without explicit flags",
			flags: &runExecFlags{autoApprove: true, defaultSafety: session.SafetyPolicyBalanced},
			want:  session.SafetyPolicyBalanced,
		},
		{
			name:  "explicit --yolo=false is not an explicit mode",
			flags: &runExecFlags{yoloChanged: true, defaultSafety: session.SafetyPolicyBalanced},
			want:  session.SafetyPolicyBalanced,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			flags := tt.flags
			if flags == nil {
				flags = &runExecFlags{}
			}
			assert.Equal(t, tt.want, flags.userSafetyPolicy())
			assert.Equal(t, tt.wantExplicit, flags.explicitCLISafety() != "")
		})
	}
}
