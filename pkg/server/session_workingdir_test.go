package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
)

// symlinkOrSkip creates a symlink, skipping the test on Windows
// environments where symlink creation requires elevated privileges.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating symlinks requires SeCreateSymbolicLinkPrivilege or Developer Mode: %v", err)
		}
		t.Fatalf("creating symlink: %v", err)
	}
}

// TestCreateSession_WorkingDirUnrestrictedDefault pins the compatible
// default: without WithSessionWorkingDirRoot, POST /api/sessions may point
// a session at any existing host directory, stored exactly as supplied
// (absolute, not canonicalised), and neither the process cwd nor
// runConfig.WorkingDir acts as an implicit boundary (#3788).
func TestCreateSession_WorkingDirUnrestrictedDefault(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	elsewhere := t.TempDir()

	newSM := func(rc *config.RuntimeConfig) *SessionManager {
		return NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, rc)
	}

	t.Run("arbitrary directory is accepted and stored as-is", func(t *testing.T) {
		t.Parallel()
		sm := newSM(&config.RuntimeConfig{})
		created, err := sm.CreateSession(ctx, &session.Session{WorkingDir: elsewhere})
		require.NoError(t, err)
		assert.Equal(t, elsewhere, created.WorkingDir)
	})

	t.Run("runConfig.WorkingDir is not an implicit boundary", func(t *testing.T) {
		t.Parallel()
		// Regression guard for the #3758 revert: --working-dir is a default
		// cwd for tools, never a containment root.
		rc := &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}
		sm := newSM(rc)
		created, err := sm.CreateSession(ctx, &session.Session{WorkingDir: elsewhere})
		require.NoError(t, err)
		assert.Equal(t, elsewhere, created.WorkingDir)
	})

	t.Run("empty working_dir is accepted", func(t *testing.T) {
		t.Parallel()
		sm := newSM(&config.RuntimeConfig{})
		created, err := sm.CreateSession(ctx, &session.Session{})
		require.NoError(t, err)
		assert.Empty(t, created.WorkingDir)
	})

	t.Run("non-existent working_dir is rejected", func(t *testing.T) {
		t.Parallel()
		sm := newSM(&config.RuntimeConfig{})
		_, err := sm.CreateSession(ctx, &session.Session{WorkingDir: filepath.Join(elsewhere, "does-not-exist")})
		require.Error(t, err)
	})

	t.Run("working_dir that is a file is rejected", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(elsewhere, "afile.txt")
		require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
		sm := newSM(&config.RuntimeConfig{})
		_, err := sm.CreateSession(ctx, &session.Session{WorkingDir: f})
		require.Error(t, err)
		assert.EqualError(t, err, "working directory must be a directory")
	})
}

// TestCreateSession_WorkingDirRootConfigured covers the opt-in containment
// enabled by WithSessionWorkingDirRoot (go/path-injection, CodeQL alert #57).
func TestCreateSession_WorkingDirRootConfigured(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	root := t.TempDir()

	newSM := func() *SessionManager {
		return NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{},
			WithSessionWorkingDirRoot(root))
	}

	t.Run("empty working_dir is accepted without applying the root", func(t *testing.T) {
		t.Parallel()
		sm := newSM()
		created, err := sm.CreateSession(ctx, &session.Session{})
		require.NoError(t, err)
		assert.Empty(t, created.WorkingDir)
	})

	t.Run("working_dir equal to root is accepted and canonicalised", func(t *testing.T) {
		t.Parallel()
		sm := newSM()
		created, err := sm.CreateSession(ctx, &session.Session{WorkingDir: root})
		require.NoError(t, err)
		resolvedRoot, err := filepath.EvalSymlinks(root)
		require.NoError(t, err)
		assert.Equal(t, resolvedRoot, created.WorkingDir)
	})

	t.Run("working_dir inside root is accepted and canonicalised", func(t *testing.T) {
		t.Parallel()
		sub := filepath.Join(root, "sub")
		require.NoError(t, os.Mkdir(sub, 0o755))

		sm := newSM()
		created, err := sm.CreateSession(ctx, &session.Session{WorkingDir: sub})
		require.NoError(t, err)
		resolvedSub, err := filepath.EvalSymlinks(sub)
		require.NoError(t, err)
		assert.Equal(t, resolvedSub, created.WorkingDir)
	})

	t.Run("sibling directory outside root is rejected", func(t *testing.T) {
		t.Parallel()
		outside := t.TempDir()
		sm := newSM()
		_, err := sm.CreateSession(ctx, &session.Session{WorkingDir: outside})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the permitted root")
	})

	t.Run("dot-dot traversal escaping root is rejected", func(t *testing.T) {
		t.Parallel()
		sm := newSM()
		_, err := sm.CreateSession(ctx, &session.Session{WorkingDir: filepath.Join(root, "..")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the permitted root")
	})

	t.Run("symlink inside root pointing outside root is rejected", func(t *testing.T) {
		t.Parallel()
		outside := t.TempDir()
		link := filepath.Join(root, "escape-link")
		symlinkOrSkip(t, outside, link)

		sm := newSM()
		_, err := sm.CreateSession(ctx, &session.Session{WorkingDir: link})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the permitted root")
	})

	t.Run("symlink inside root pointing inside root is accepted resolved", func(t *testing.T) {
		t.Parallel()
		target := filepath.Join(root, "target")
		require.NoError(t, os.Mkdir(target, 0o755))
		link := filepath.Join(root, "inner-link")
		symlinkOrSkip(t, target, link)

		sm := newSM()
		created, err := sm.CreateSession(ctx, &session.Session{WorkingDir: link})
		require.NoError(t, err)
		resolvedTarget, err := filepath.EvalSymlinks(target)
		require.NoError(t, err)
		assert.Equal(t, resolvedTarget, created.WorkingDir)
	})

	t.Run("non-existent working_dir is rejected", func(t *testing.T) {
		t.Parallel()
		sm := newSM()
		_, err := sm.CreateSession(ctx, &session.Session{WorkingDir: filepath.Join(root, "does-not-exist")})
		require.Error(t, err)
	})

	t.Run("working_dir that is a file is rejected", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(root, "afile.txt")
		require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))

		sm := newSM()
		_, err := sm.CreateSession(ctx, &session.Session{WorkingDir: f})
		require.Error(t, err)
		assert.EqualError(t, err, "working directory must be a directory")
	})
}

// TestCreateSession_WorkingDirWhitespaceRoot: a root that trims to empty is
// a misconfiguration and must fail instead of silently disabling containment.
func TestCreateSession_WorkingDirWhitespaceRoot(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{},
		WithSessionWorkingDirRoot("   "))

	_, err := sm.CreateSession(ctx, &session.Session{WorkingDir: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitespace")
}

// TestResolveWithinRoot exercises the containment helper directly.
func TestResolveWithinRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	sm := NewSessionManager(t.Context(), config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{},
		WithSessionWorkingDirRoot(root))

	t.Run("path equal to root is allowed", func(t *testing.T) {
		t.Parallel()
		resolvedRoot, err := filepath.EvalSymlinks(root)
		require.NoError(t, err)
		got, err := sm.resolveWithinRoot(root)
		require.NoError(t, err)
		assert.Equal(t, resolvedRoot, got)
	})

	t.Run("subpath is allowed", func(t *testing.T) {
		t.Parallel()
		sub := filepath.Join(root, "child")
		require.NoError(t, os.Mkdir(sub, 0o755))
		resolvedSub, err := filepath.EvalSymlinks(sub)
		require.NoError(t, err)
		got, err := sm.resolveWithinRoot(sub)
		require.NoError(t, err)
		assert.Equal(t, resolvedSub, got)
	})

	t.Run("sibling outside root is rejected", func(t *testing.T) {
		t.Parallel()
		sibling := t.TempDir()
		_, err := sm.resolveWithinRoot(sibling)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the permitted root")
	})

	t.Run("non-existent path returns error", func(t *testing.T) {
		t.Parallel()
		_, err := sm.resolveWithinRoot(filepath.Join(root, "ghost"))
		require.Error(t, err)
	})

	t.Run("symlink inside root pointing outside root is rejected", func(t *testing.T) {
		t.Parallel()
		outside := t.TempDir()
		link := filepath.Join(root, "evil-link")
		symlinkOrSkip(t, outside, link)

		_, err := sm.resolveWithinRoot(link)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the permitted root")
	})

	t.Run("dot-dot traversal that resolves outside root is rejected", func(t *testing.T) {
		t.Parallel()
		// Build a sub-dir so that three ".." hops escape root and land at a
		// real ancestor directory (/tmp or similar). filepath.Abs cleans the
		// ".." components; the result is an existing path outside root, so
		// this exercises the containment check itself (not just EvalSymlinks
		// failing on a non-existent path).
		sub := filepath.Join(root, "dotdot-child")
		require.NoError(t, os.Mkdir(sub, 0o755))
		escaped, err := filepath.Abs(filepath.Join(sub, "..", "..", ".."))
		require.NoError(t, err)

		_, err = sm.resolveWithinRoot(escaped)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the permitted root")
	})

	t.Run("whitespace-only root is an error, not a bypass", func(t *testing.T) {
		t.Parallel()
		wsSM := NewSessionManager(t.Context(), config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{},
			WithSessionWorkingDirRoot(" \t"))
		_, err := wsSM.resolveWithinRoot(t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "whitespace")
	})
}

// TestResolveWithinRoot_NoRootConfigured: without a configured root the path
// is returned untouched — no canonicalisation, no cwd/--working-dir boundary.
func TestResolveWithinRoot_NoRootConfigured(t *testing.T) {
	t.Parallel()

	unrelated := t.TempDir()

	cases := []struct {
		name string
		rc   *config.RuntimeConfig
	}{
		{"empty runConfig", &config.RuntimeConfig{}},
		{"runConfig.WorkingDir set", &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sm := NewSessionManager(t.Context(), config.Sources{}, session.NewInMemorySessionStore(), 0, tc.rc)
			got, err := sm.resolveWithinRoot(unrelated)
			require.NoError(t, err)
			assert.Equal(t, unrelated, got)
		})
	}
}
