package reference

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOciRefToFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"ordinary ref with tag", "docker.io/myorg/agent:v1", "docker.io_myorg_agent_v1.yaml"},
		{"registry with port", "localhost:5000/test", "localhost_5000_test.yaml"},
		{"ref with digest", "myorg/agent@sha256:deadbeef", "myorg_agent_sha256_deadbeef.yaml"},
		{"port tag and digest", "registry.example.com:443/org/agent:v2@sha256:abc123", "registry.example.com_443_org_agent_v2_sha256_abc123.yaml"},
		{"no tag", "myorg/agent", "myorg_agent.yaml"},
		{"slash", "a/b", "a_b.yaml"},
		{"colon", "a:b", "a_b.yaml"},
		{"at sign", "a@b", "a_b.yaml"},
		{"backslash", `a\b`, "a_b.yaml"},
		{"asterisk", "a*b", "a_b.yaml"},
		{"question mark", "a?b", "a_b.yaml"},
		{"double quote", `a"b`, "a_b.yaml"},
		{"less than", "a<b", "a_b.yaml"},
		{"greater than", "a>b", "a_b.yaml"},
		{"pipe", "a|b", "a_b.yaml"},
		{"all replaced characters", `/:@\*?"<>|`, "__________.yaml"},
		{"empty input", "", ".yaml"},
		{"existing .yaml suffix", "agent.yaml", "agent.yaml"},
		{"ref with .yaml suffix", "myorg/agent.yaml", "myorg_agent.yaml"},
		{"safe characters kept", "already_safe-name.v1", "already_safe-name.v1.yaml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, OciRefToFilename(tt.ref))
		})
	}
}

// FuzzOciRefToFilename checks structural invariants only: the converter is
// lossy (every replaced character maps to "_"), so no reverse parse exists.
func FuzzOciRefToFilename(f *testing.F) {
	seeds := []string{
		"docker.io/myorg/agent:v1",
		"localhost:5000/test",
		"myorg/agent@sha256:deadbeef",
		"",
		".yaml",
		"agent.yaml",
		`/:@\*?"<>|`,
		"already_safe",
		"héllo/wörld:tag",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, ref string) {
		got := OciRefToFilename(ref)

		if !strings.HasSuffix(got, ".yaml") {
			t.Errorf("OciRefToFilename(%q) = %q, want .yaml suffix", ref, got)
		}
		if strings.ContainsAny(got, `/:@\*?"<>|`) {
			t.Errorf("OciRefToFilename(%q) = %q, contains a character that should have been replaced", ref, got)
		}
		if again := OciRefToFilename(got); again != got {
			t.Errorf("OciRefToFilename is not idempotent: f(%q) = %q, f(f) = %q", ref, got, again)
		}
	})
}
