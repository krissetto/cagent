package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsTruthy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"on", true},
		{" true ", true},
		{"0", false},
		{"false", false},
		{"", false},
		{"nope", false},
	} {
		assert.Equal(t, tc.want, isTruthy(tc.in), "input %q", tc.in)
	}
}

func TestEnabled(t *testing.T) {
	t.Setenv(EnvAutoUpdate, "")
	t.Setenv(envReExecMarker, "")
	assert.False(t, Enabled())

	t.Setenv(EnvAutoUpdate, "true")
	assert.True(t, Enabled())

	// The re-exec marker disables updates even when explicitly enabled.
	t.Setenv(envReExecMarker, "1")
	assert.False(t, Enabled())
}

func TestAssetName(t *testing.T) {
	t.Parallel()

	u := &Updater{Repo: "docker-agent", GOOS: "linux", GOARCH: "amd64"}
	assert.Equal(t, "docker-agent-linux-amd64", u.assetName())

	u = &Updater{Repo: "docker-agent", GOOS: "windows", GOARCH: "arm64"}
	assert.Equal(t, "docker-agent-windows-arm64.exe", u.assetName())
}

func TestParseSHA256Digest(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("payload"))
	hexSum := hex.EncodeToString(sum[:])

	got, err := parseSHA256Digest("sha256:" + hexSum)
	require.NoError(t, err)
	assert.Equal(t, hexSum, got)

	got, err = parseSHA256Digest("sha256:" + strings.ToUpper(hexSum))
	require.NoError(t, err)
	assert.Equal(t, hexSum, got, "digest hex must be normalized to lowercase")

	for name, digest := range map[string]string{
		"empty":           "",
		"no algorithm":    hexSum,
		"wrong algorithm": "sha512:" + hexSum,
		"not hex":         "sha256:not-hex-at-all",
		"truncated":       "sha256:" + hexSum[:32],
		"trailing junk":   "sha256:" + hexSum + "ff",
	} {
		_, err := parseSHA256Digest(digest)
		assert.Error(t, err, "case %s: %q", name, digest)
	}
}

func TestVerifyChecksum(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("payload"))
	hexSum := hex.EncodeToString(sum[:])

	require.NoError(t, verifyChecksum(releaseInfo{Asset: testAssetName, SHA256: hexSum}, hexSum))
	require.NoError(t, verifyChecksum(releaseInfo{Asset: testAssetName, SHA256: strings.ToUpper(hexSum)}, hexSum),
		"digest comparison must be case-insensitive")

	require.Error(t, verifyChecksum(releaseInfo{Asset: testAssetName}, hexSum), "empty digest must fail closed")

	err := verifyChecksum(releaseInfo{Asset: testAssetName, SHA256: hexSum}, strings.Repeat("0", 64))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")
}

const testAssetName = "docker-agent-plan9-mips"

func sha256Digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// newFakeRelease returns an httptest server emulating the GitHub release API
// and download endpoints for the given tag and asset payload, plus a counter
// of requests hitting the asset download endpoint. The digest is reported
// verbatim in the API response; pass sha256Digest(payload) for a valid
// release.
func newFakeRelease(t *testing.T, tag string, payload []byte, digest string) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	assetName := testAssetName

	var downloads atomic.Int64
	var baseURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/docker/docker-agent/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q,"digest":%q}]}`, tag, assetName, baseURL+"/docker/docker-agent/releases/download/"+tag+"/"+assetName, digest)
	})
	mux.HandleFunc("/docker/docker-agent/releases/download/"+tag+"/"+assetName, func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(payload)
	})

	srv := httptest.NewServer(mux)
	baseURL = srv.URL
	t.Cleanup(srv.Close)
	return srv, &downloads
}

// newTestUpdater wires an Updater against srv, targeting a non-host platform so
// verifyBinary is skipped, and capturing the re-exec call instead of execing.
func newTestUpdater(t *testing.T, srv *httptest.Server, currentVer, exePath string) (*Updater, *reExecCapture) {
	t.Helper()

	capt := &reExecCapture{}
	return &Updater{
		CurrentVersion:  currentVer,
		Owner:           "docker",
		Repo:            "docker-agent",
		APIBaseURL:      srv.URL,
		DownloadBaseURL: srv.URL,
		// Deliberately not the host platform: verifyBinary returns early so we
		// don't try to exec a fake binary.
		GOOS:       "plan9",
		GOARCH:     "mips",
		HTTPClient: srv.Client(),
		resolveExecutable: func() (string, error) {
			return exePath, nil
		},
		reExec:  capt.fn,
		install: installExecutable,
		confirm: func(io.Reader, io.Writer, string, string) bool { return true },
	}, capt
}

type reExecCapture struct {
	mu     sync.Mutex
	called bool
	path   string
	args   []string
	env    []string
}

func (c *reExecCapture) fn(path string, args, env []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.called = true
	c.path = path
	c.args = args
	c.env = env
	return nil
}

func TestTryUpdateSuccess(t *testing.T) {
	t.Parallel()
	payload := []byte("#!/bin/sh\necho new binary\n")
	srv, _ := newFakeRelease(t, "v2.0.0", payload, sha256Digest(payload))

	dir := t.TempDir()
	exePath := filepath.Join(dir, "docker-agent")
	require.NoError(t, os.WriteFile(exePath, []byte("old binary"), 0o755))

	u, capt := newTestUpdater(t, srv, "v1.0.0", exePath)

	var stderr strings.Builder
	require.NoError(t, u.tryUpdate(t.Context(), nil, &stderr))

	// The on-disk binary was replaced with the downloaded payload.
	got, err := os.ReadFile(exePath)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	// And the new binary was re-executed with the loop-guard env marker.
	assert.True(t, capt.called)
	assert.Equal(t, exePath, capt.path)
	assert.Contains(t, capt.env, envReExecMarker+"=1")
}

func TestTryUpdateUppercaseDigest(t *testing.T) {
	t.Parallel()
	payload := []byte("#!/bin/sh\necho new binary\n")
	sum := sha256.Sum256(payload)
	srv, _ := newFakeRelease(t, "v2.0.0", payload, "sha256:"+strings.ToUpper(hex.EncodeToString(sum[:])))

	dir := t.TempDir()
	exePath := filepath.Join(dir, "docker-agent")
	require.NoError(t, os.WriteFile(exePath, []byte("old binary"), 0o755))

	u, capt := newTestUpdater(t, srv, "v1.0.0", exePath)

	var stderr strings.Builder
	require.NoError(t, u.tryUpdate(t.Context(), nil, &stderr), "an uppercase hex digest must verify")

	got, err := os.ReadFile(exePath)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	assert.True(t, capt.called)
}

func TestTryUpdateDeclinedDoesNotUpdate(t *testing.T) {
	t.Parallel()
	payload := []byte("#!/bin/sh\necho new binary\n")
	srv, _ := newFakeRelease(t, "v2.0.0", payload, sha256Digest(payload))

	dir := t.TempDir()
	exePath := filepath.Join(dir, "docker-agent")
	require.NoError(t, os.WriteFile(exePath, []byte("old binary"), 0o755))

	u, capt := newTestUpdater(t, srv, "v1.0.0", exePath)
	u.confirm = func(io.Reader, io.Writer, string, string) bool { return false }

	var stderr strings.Builder
	require.NoError(t, u.tryUpdate(t.Context(), nil, &stderr))

	assert.False(t, capt.called, "declining must not re-exec")
	got, _ := os.ReadFile(exePath)
	assert.Equal(t, "old binary", string(got), "binary must be untouched when declined")
}

func TestConfirmUpdateNonInteractiveAutoConfirms(t *testing.T) {
	t.Parallel()

	// A non-*os.File reader (e.g. a pipe in CI) is non-interactive: auto-confirm.
	var stderr strings.Builder
	assert.True(t, confirmUpdate(strings.NewReader(""), &stderr, "v1.0.0", "v2.0.0"))
	assert.Empty(t, stderr.String(), "must not prompt in a non-interactive session")
}

func TestAnswerIsYes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true}, // default is yes
		{"\n", true},
		{"y", true},
		{"Y", true},
		{"yes", true},
		{" YES ", true},
		{"n", false},
		{"no", false},
		{"nope", false},
		{"x", false},
	} {
		assert.Equal(t, tc.want, answerIsYes(tc.in), "input %q", tc.in)
	}
}

func TestTryUpdateAlreadyLatest(t *testing.T) {
	t.Parallel()
	srv, _ := newFakeRelease(t, "v1.0.0", []byte("x"), sha256Digest([]byte("x")))

	dir := t.TempDir()
	exePath := filepath.Join(dir, "docker-agent")
	require.NoError(t, os.WriteFile(exePath, []byte("old"), 0o755))

	u, capt := newTestUpdater(t, srv, "v1.0.0", exePath)

	var stderr strings.Builder
	require.NoError(t, u.tryUpdate(t.Context(), nil, &stderr))

	assert.False(t, capt.called, "should not re-exec when already up to date")
	got, _ := os.ReadFile(exePath)
	assert.Equal(t, "old", string(got), "binary must be untouched")
}

func TestTryUpdateDevVersionNeverUpdates(t *testing.T) {
	t.Parallel()
	srv, _ := newFakeRelease(t, "v1.0.0", []byte("x"), sha256Digest([]byte("x")))

	dir := t.TempDir()
	exePath := filepath.Join(dir, "docker-agent")
	require.NoError(t, os.WriteFile(exePath, []byte("old"), 0o755))

	u, capt := newTestUpdater(t, srv, devVersion, exePath)

	var stderr strings.Builder
	err := u.tryUpdate(t.Context(), nil, &stderr)
	require.Error(t, err, "dev builds must not be replaced")
	assert.False(t, capt.called)
}

func TestTryUpdateChecksumMismatch(t *testing.T) {
	t.Parallel()
	payload := []byte("real payload")

	dir := t.TempDir()
	exePath := filepath.Join(dir, "docker-agent")
	require.NoError(t, os.WriteFile(exePath, []byte("old"), 0o755))

	// Server advertises a well-formed digest that does not match the payload.
	srv, _ := newFakeRelease(t, "v2.0.0", payload, sha256Digest([]byte("something else")))

	u, capt := newTestUpdater(t, srv, "v1.0.0", exePath)

	var stderr strings.Builder
	err := u.tryUpdate(t.Context(), nil, &stderr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")
	assert.False(t, capt.called)

	// The original binary must be intact on failure.
	got, _ := os.ReadFile(exePath)
	assert.Equal(t, "old", string(got))
}

func TestTryUpdateBadDigestFailsClosed(t *testing.T) {
	t.Parallel()
	payload := []byte("real payload")
	sum := sha256.Sum256(payload)
	hexSum := hex.EncodeToString(sum[:])

	for name, digest := range map[string]string{
		"missing":         "",
		"no algorithm":    hexSum,
		"wrong algorithm": "sha512:" + hexSum,
		"not hex":         "sha256:not-hex-at-all",
		"truncated":       "sha256:" + hexSum[:32],
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, downloads := newFakeRelease(t, "v2.0.0", payload, digest)

			dir := t.TempDir()
			exePath := filepath.Join(dir, "docker-agent")
			require.NoError(t, os.WriteFile(exePath, []byte("old"), 0o755))

			u, capt := newTestUpdater(t, srv, "v1.0.0", exePath)

			var stderr strings.Builder
			err := u.tryUpdate(t.Context(), nil, &stderr)
			require.Error(t, err, "digest %q must fail closed", digest)
			assert.Contains(t, err.Error(), "digest")
			assert.False(t, capt.called)
			assert.Zero(t, downloads.Load(), "a bad digest must be rejected before the asset is requested")

			got, err := os.ReadFile(exePath)
			require.NoError(t, err)
			assert.Equal(t, "old", string(got), "binary must be untouched")
		})
	}
}

func TestTryUpdateReExecFailureRestoresPreviousBinary(t *testing.T) {
	t.Parallel()
	payload := []byte("new binary")
	srv, _ := newFakeRelease(t, "v2.0.0", payload, sha256Digest(payload))

	dir := t.TempDir()
	exePath := filepath.Join(dir, "docker-agent")
	require.NoError(t, os.WriteFile(exePath, []byte("old binary"), 0o755))

	u, _ := newTestUpdater(t, srv, "v1.0.0", exePath)
	u.reExec = func(string, []string, []string) error {
		return errors.New("boom")
	}

	var stderr strings.Builder
	err := u.tryUpdate(t.Context(), nil, &stderr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "re-executing updated binary")

	got, err := os.ReadFile(exePath)
	require.NoError(t, err)
	assert.Equal(t, "old binary", string(got))
}

func TestTryUpdateDownloadNotFound(t *testing.T) {
	t.Parallel()
	// Latest resolves but the asset 404s: must fail and leave binary intact.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			fmt.Fprintf(w, `{"tag_name":"v2.0.0","assets":[{"name":"docker-agent-plan9-mips","browser_download_url":%q,"digest":%q}]}`,
				"http://"+r.Host+"/docker/docker-agent/releases/download/v2.0.0/docker-agent-plan9-mips", sha256Digest([]byte("x")))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	exePath := filepath.Join(dir, "docker-agent")
	require.NoError(t, os.WriteFile(exePath, []byte("old"), 0o755))

	u, capt := newTestUpdater(t, srv, "v1.0.0", exePath)

	var stderr strings.Builder
	err := u.tryUpdate(t.Context(), nil, &stderr)
	require.Error(t, err)
	assert.False(t, capt.called)
	got, _ := os.ReadFile(exePath)
	assert.Equal(t, "old", string(got))
}

func TestRunSwallowsErrors(t *testing.T) {
	t.Parallel()
	// A totally unreachable server must not panic or propagate: Run is
	// best-effort and only logs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	exePath := filepath.Join(dir, "docker-agent")
	require.NoError(t, os.WriteFile(exePath, []byte("old"), 0o755))

	u, capt := newTestUpdater(t, srv, "v1.0.0", exePath)

	var stderr strings.Builder
	u.run(t.Context(), nil, &stderr) // must not panic
	assert.False(t, capt.called)
	assert.Contains(t, stderr.String(), "self-update failed")
}

func TestLatestReleaseAuthHeader(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret-token")

	sum := sha256.Sum256([]byte("payload"))
	hexSum := hex.EncodeToString(sum[:])
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":"docker-agent-plan9-mips","browser_download_url":%q,"digest":%q}]}`,
			"http://"+r.Host+"/docker/docker-agent/releases/download/v9.9.9/docker-agent-plan9-mips", "sha256:"+hexSum)
	}))
	t.Cleanup(srv.Close)

	u := &Updater{
		Owner:           "docker",
		Repo:            "docker-agent",
		APIBaseURL:      srv.URL,
		DownloadBaseURL: srv.URL,
		HTTPClient:      srv.Client(),
	}

	release, err := u.latestRelease(t.Context(), "docker-agent-plan9-mips")
	require.NoError(t, err)
	assert.Equal(t, "v9.9.9", release.Tag)
	assert.Equal(t, hexSum, release.SHA256, "latestRelease must keep the normalized asset digest")
	assert.Equal(t, "Bearer secret-token", gotAuth)
}

func TestLatestReleaseRejectsBadDigest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":"docker-agent-plan9-mips","browser_download_url":%q,"digest":"sha512:abc"}]}`,
			"http://"+r.Host+"/docker/docker-agent/releases/download/v9.9.9/docker-agent-plan9-mips")
	}))
	t.Cleanup(srv.Close)

	u := &Updater{
		Owner:           "docker",
		Repo:            "docker-agent",
		APIBaseURL:      srv.URL,
		DownloadBaseURL: srv.URL,
		HTTPClient:      srv.Client(),
	}

	_, err := u.latestRelease(t.Context(), "docker-agent-plan9-mips")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported digest")
}

func TestLatestReleaseRejectsUntrustedDownloadHost(t *testing.T) {
	t.Parallel()
	// The asset download URL points at an attacker-controlled host while the
	// trusted DownloadBaseURL is the test server: resolution must fail rather
	// than follow the foreign URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":"docker-agent-plan9-mips","browser_download_url":%q}]}`, "http://evil.example.com/docker-agent-plan9-mips")
	}))
	t.Cleanup(srv.Close)

	u := &Updater{
		Owner:           "docker",
		Repo:            "docker-agent",
		APIBaseURL:      srv.URL,
		DownloadBaseURL: srv.URL,
		HTTPClient:      srv.Client(),
	}

	_, err := u.latestRelease(t.Context(), "docker-agent-plan9-mips")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the trusted host")
}

func TestValidateDownloadURL(t *testing.T) {
	t.Parallel()

	u := &Updater{Owner: "docker", Repo: "docker-agent", DownloadBaseURL: "https://github.com"}
	const tag, asset = "v1.119.0", "docker-agent-linux-amd64"
	const good = "https://github.com/docker/docker-agent/releases/download/v1.119.0/docker-agent-linux-amd64"

	require.NoError(t, u.validateDownloadURL(good, tag, asset))
	require.NoError(t, u.validateDownloadURL("https://GitHub.com/docker/docker-agent/releases/download/v1.119.0/docker-agent-linux-amd64", tag, asset),
		"host comparison must be case-insensitive")
	require.NoError(t, u.validateDownloadURL("https://github.com/docker/docker-agent/releases/download/v1.119.0/docker%2Dagent-linux-amd64", tag, asset),
		"escaped-but-equivalent path segments must be accepted")

	for name, raw := range map[string]string{
		"foreign host":       "https://objects.githubusercontent.com/docker/docker-agent/releases/download/v1.119.0/docker-agent-linux-amd64",
		"non-HTTPS":          "http://github.com/docker/docker-agent/releases/download/v1.119.0/docker-agent-linux-amd64",
		"userinfo":           "https://user:pass@github.com/docker/docker-agent/releases/download/v1.119.0/docker-agent-linux-amd64",
		"unexpected port":    "https://github.com:8443/docker/docker-agent/releases/download/v1.119.0/docker-agent-linux-amd64",
		"query":              good + "?x=1",
		"fragment":           good + "#frag",
		"other owner":        "https://github.com/evil/docker-agent/releases/download/v1.119.0/docker-agent-linux-amd64",
		"other repo":         "https://github.com/docker/evil/releases/download/v1.119.0/docker-agent-linux-amd64",
		"other tag":          "https://github.com/docker/docker-agent/releases/download/v9.9.9/docker-agent-linux-amd64",
		"other asset":        "https://github.com/docker/docker-agent/releases/download/v1.119.0/evil",
		"extra segment":      good + "/extra",
		"missing segment":    "https://github.com/docker/docker-agent/releases/download/v1.119.0",
		"not a release path": "https://github.com/docker/docker-agent/archive/v1.119.0/docker-agent-linux-amd64",
		"encoded slash":      "https://github.com/docker/docker-agent/releases/download/v1.119.0%2Fdocker-agent-linux-amd64",
		"unparseable":        "://bad",
	} {
		assert.Error(t, u.validateDownloadURL(raw, tag, asset), "case %s: %s", name, raw)
	}
}

func TestNewHTTPClientRedirectPolicy(t *testing.T) {
	t.Parallel()

	check := New("v1.0.0").HTTPClient.CheckRedirect
	require.NotNil(t, check, "production client must restrict redirects")

	mustParse := func(s string) *url.URL {
		u, err := url.Parse(s)
		require.NoError(t, err)
		return u
	}

	require.NoError(t, check(&http.Request{URL: mustParse("https://objects.githubusercontent.com/asset")}, make([]*http.Request, 1)))

	err := check(&http.Request{URL: mustParse("http://github.com/asset")}, make([]*http.Request, 1))
	require.Error(t, err, "HTTP downgrade must be refused")
	assert.Contains(t, err.Error(), "non-https")

	err = check(&http.Request{URL: mustParse("https://github.com/asset")}, make([]*http.Request, 10))
	require.Error(t, err, "redirect chain must be bounded")
	assert.Contains(t, err.Error(), "10 redirects")
}

func TestSelfUpdateEnvStripsMarkers(t *testing.T) {
	t.Parallel()

	in := []string{
		"PATH=/usr/bin",
		envBackupMarker + "=/tmp/stale",
		"HOME=/home/me",
		envReExecMarker + "=1",
	}

	got := selfUpdateEnv(in)
	assert.Equal(t, []string{"PATH=/usr/bin", "HOME=/home/me"}, got)

	// Appending fresh markers must yield exactly one entry for each key.
	full := append(selfUpdateEnv(in), envReExecMarker+"=1", envBackupMarker+"=/tmp/new")
	assert.Equal(t, 1, countKey(full, envReExecMarker))
	assert.Equal(t, 1, countKey(full, envBackupMarker))
	assert.Contains(t, full, envBackupMarker+"=/tmp/new")
}

func countKey(env []string, key string) int {
	n := 0
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			n++
		}
	}
	return n
}

func TestVerifyEnv(t *testing.T) {
	t.Parallel()

	fakeEnv := func(vars map[string]string) func(string) string {
		return func(key string) string { return vars[key] }
	}

	// Secrets and proxy settings must never reach the probe: the env is an
	// allowlist, so asserting the exact result proves nothing else leaks.
	secrets := map[string]string{
		"HOME":           "/home/me",
		"GITHUB_TOKEN":   "gh-secret",
		"GH_TOKEN":       "gh-secret",
		"OPENAI_API_KEY": "sk-secret",
		"HTTPS_PROXY":    "http://user:pass@proxy:3128",
		"PATH":           "/usr/bin",
	}
	assert.Equal(t, []string{envReExecMarker + "=1", "HOME=/home/me"}, verifyEnv("darwin", fakeEnv(secrets)))
	assert.Equal(t, []string{envReExecMarker + "=1", "HOME=/home/me"}, verifyEnv("linux", fakeEnv(secrets)))

	// Unset and empty variables are dropped; the loop-guard marker always stays.
	assert.Equal(t, []string{envReExecMarker + "=1"}, verifyEnv("linux", fakeEnv(nil)))
	assert.Equal(t, []string{envReExecMarker + "=1"}, verifyEnv("darwin", fakeEnv(map[string]string{"HOME": ""})))

	// Windows additionally passes through the system variables the probe
	// needs to start, still excluding everything else.
	got := verifyEnv("windows", fakeEnv(map[string]string{
		"HOME":         `C:\home`,
		"SYSTEMROOT":   `C:\Windows`,
		"PATH":         `C:\Windows\system32`,
		"USERPROFILE":  `C:\Users\me`,
		"ProgramData":  `C:\ProgramData`,
		"GITHUB_TOKEN": "gh-secret",
	}))
	assert.Equal(t, []string{
		envReExecMarker + "=1",
		`HOME=C:\home`,
		`SYSTEMROOT=C:\Windows`,
		`PATH=C:\Windows\system32`,
		`USERPROFILE=C:\Users\me`,
		`ProgramData=C:\ProgramData`,
	}, got)

	assert.Equal(t, []string{envReExecMarker + "=1", `SYSTEMROOT=C:\Windows`},
		verifyEnv("windows", fakeEnv(map[string]string{"SYSTEMROOT": `C:\Windows`})),
		"unset Windows variables must be dropped, not passed empty")
}

// TestVerifyBinaryProvidesHome reproduces the live blocker: the "version"
// subcommand resolves the user home directory during telemetry/desktop
// initialization and dies without HOME on macOS. The stand-in script fails
// exactly when one of the probe-environment guarantees is violated.
func TestVerifyBinaryProvidesHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("probe stand-in is a shell script")
	}

	script := filepath.Join(t.TempDir(), "docker-agent")
	require.NoError(t, os.WriteFile(script, fmt.Appendf(nil, `#!/bin/sh
[ "$1" = version ] || exit 13
[ -n "$HOME" ] || exit 10
[ -z "$GITHUB_TOKEN" ] || exit 11
[ "$%s" = 1 ] || exit 12
echo v9.9.9
`, envReExecMarker), 0o755))

	// Host platform so verifyBinary actually runs the probe.
	u := &Updater{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "gh-secret")
	require.NoError(t, u.verifyBinary(t.Context(), script))

	t.Setenv("HOME", "")
	err := u.verifyBinary(t.Context(), script)
	require.Error(t, err, "probe must fail without HOME, like the version command on macOS")
	assert.Contains(t, err.Error(), "staged binary failed to run")
}

func TestCleanupRemovesBackup(t *testing.T) {
	backup := filepath.Join(t.TempDir(), backupFilePrefix+"123")
	require.NoError(t, os.WriteFile(backup, []byte("old"), 0o755))
	t.Setenv(envBackupMarker, backup)

	Cleanup(t.Context())

	_, err := os.Stat(backup)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCleanupIgnoresForeignBackupPath(t *testing.T) {
	// A path that does not look like one of our backups must never be removed,
	// even if pointed at by the environment variable.
	victim := filepath.Join(t.TempDir(), "important.txt")
	require.NoError(t, os.WriteFile(victim, []byte("keep"), 0o644))
	t.Setenv(envBackupMarker, victim)

	Cleanup(t.Context())

	got, err := os.ReadFile(victim)
	require.NoError(t, err, "foreign path must not be deleted")
	assert.Equal(t, "keep", string(got))
}

func TestIsOwnedBackupPath(t *testing.T) {
	t.Parallel()

	assert.True(t, isOwnedBackupPath("/tmp/"+backupFilePrefix+"abc"))
	assert.True(t, isOwnedBackupPath(backupFilePrefix+"abc"))
	assert.False(t, isOwnedBackupPath("/tmp/important.txt"))
	assert.False(t, isOwnedBackupPath("/etc/passwd"))
	assert.False(t, isOwnedBackupPath(""))
}

func TestSwapBinary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dst := filepath.Join(dir, "docker-agent")
	src := filepath.Join(dir, "staged")
	require.NoError(t, os.WriteFile(dst, []byte("old"), 0o755))
	require.NoError(t, os.WriteFile(src, []byte("new"), 0o755))

	require.NoError(t, swapBinary(dst, src))

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}
