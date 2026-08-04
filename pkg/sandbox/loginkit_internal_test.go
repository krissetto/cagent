package sandbox

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/paths"
)

func TestStaleLoginKit(t *testing.T) {
	cacheDir := t.TempDir()
	paths.SetCacheDir(cacheDir)
	t.Cleanup(func() { paths.SetCacheDir("") })

	kitA := filepath.Join(cacheDir, "sandbox-login-kit", "a.docker.com")
	kitB := filepath.Join(cacheDir, "sandbox-login-kit", "b.docker.com")

	withKitA := &Existing{Workspaces: []string{"/ws", kitA + ":ro"}}
	withoutKit := &Existing{Workspaces: []string{"/ws", "/extra:ro"}}

	// A sandbox holding a login kit must not be reused when the run
	// wants none (its proxy would keep authenticating gateway requests
	// with the user's Docker login) or wants a different gateway host.
	assert.True(t, staleLoginKit(withKitA, ""))
	assert.True(t, staleLoginKit(withKitA, kitB))
	assert.False(t, staleLoginKit(withKitA, kitA))

	// A sandbox without any login kit is never stale on this axis;
	// a *wanted* kit is enforced by the extras mount check instead.
	assert.False(t, staleLoginKit(withoutKit, ""))
	assert.False(t, staleLoginKit(withoutKit, kitA))
}
