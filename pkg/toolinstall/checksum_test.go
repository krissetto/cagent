package toolinstall

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChecksum_IsEnabled(t *testing.T) {
	t.Parallel()

	assert.False(t, (*Checksum)(nil).isEnabled())
	assert.True(t, (&Checksum{}).isEnabled())
	assert.True(t, (&Checksum{Enabled: new(true)}).isEnabled())
	assert.False(t, (&Checksum{Enabled: new(false)}).isEnabled())
}

func TestMergeChecksum(t *testing.T) {
	t.Parallel()

	base := &Checksum{Type: "github_release", Asset: "checksums.txt", Algorithm: "sha256"}

	// Override only disables verification (common aqua per-OS pattern).
	merged := mergeChecksum(base, &Checksum{Enabled: new(false)})
	assert.Equal(t, "github_release", merged.Type)
	assert.Equal(t, "checksums.txt", merged.Asset)
	assert.False(t, merged.isEnabled())

	// Base unchanged.
	assert.True(t, base.isEnabled())

	// Nil base returns override.
	assert.Equal(t, base, mergeChecksum(nil, base))
	// Nil override returns base.
	assert.Equal(t, base, mergeChecksum(base, nil))

	// Override replaces non-empty fields.
	merged = mergeChecksum(base, &Checksum{Asset: "other.txt", Algorithm: "sha512"})
	assert.Equal(t, "other.txt", merged.Asset)
	assert.Equal(t, "sha512", merged.Algorithm)
}

func TestNewHasher(t *testing.T) {
	t.Parallel()

	for _, algo := range []string{"", "sha256", "SHA256"} {
		h, err := newHasher(algo)
		require.NoError(t, err)
		assert.Equal(t, sha256.New().Size(), h.Size())
	}

	h, err := newHasher("sha512")
	require.NoError(t, err)
	assert.Equal(t, sha512.New().Size(), h.Size())

	for _, weak := range []string{"md5", "sha1", "bogus"} {
		_, err := newHasher(weak)
		require.Error(t, err)
	}
}

func TestParseChecksumFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    string
		asset   string
		want    string
		wantErr bool
	}{
		{
			name:  "multi-entry checksums.txt",
			data:  "abc123  tool_linux_amd64.tar.gz\ndef456  tool_darwin_arm64.tar.gz\n",
			asset: "tool_darwin_arm64.tar.gz",
			want:  "def456",
		},
		{
			name:  "binary-mode marker",
			data:  "abc123 *tool_linux_amd64.tar.gz\n",
			asset: "tool_linux_amd64.tar.gz",
			want:  "abc123",
		},
		{
			name:  "match by basename",
			data:  "abc123  ./dist/tool_linux_amd64.tar.gz\n",
			asset: "tool_linux_amd64.tar.gz",
			want:  "abc123",
		},
		{
			name:  "single bare digest",
			data:  "deadbeef\n",
			asset: "anything",
			want:  "deadbeef",
		},
		{
			name:    "asset not found",
			data:    "abc123  other.tar.gz\n",
			asset:   "missing.tar.gz",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseChecksumFile([]byte(tt.data), tt.asset)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// urlRegistry builds a Registry whose HTTP client serves bodies keyed by the
// trailing path segment of the request URL, so an asset and its checksum file
// can be served distinctly.
func urlRegistry(responses map[string][]byte) *Registry {
	return &Registry{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) *http.Response {
				for name, body := range responses {
					if strings.HasSuffix(req.URL.Path, "/"+name) {
						return &http.Response{
							StatusCode: http.StatusOK,
							Body:       io.NopCloser(bytes.NewReader(body)),
						}
					}
				}
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("not found")),
				}
			}),
		},
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestInstallE2E_ChecksumVerified(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := []byte("#!/bin/sh\necho verified")
	manifest := sha256Hex(binaryContent) + "  rawtool\n"

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "rawtool",
		Format:    "raw",
		Asset:     "rawtool",
		Files:     []PackageFile{{Name: "rawtool"}},
		Checksum:  &Checksum{Type: "github_release", Asset: "checksums.txt", Algorithm: "sha256"},
	}

	registry := urlRegistry(map[string][]byte{
		"rawtool":       binaryContent,
		"checksums.txt": []byte(manifest),
	})

	result, err := registry.Install(t.Context(), pkg, "v1.0.0")
	require.NoError(t, err)
	assertInstalledBinary(t, result, string(binaryContent), "rawtool")
}

func TestInstallE2E_ChecksumMismatch(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := []byte("#!/bin/sh\necho tampered")
	// Manifest advertises a digest for different content.
	manifest := sha256Hex([]byte("original")) + "  rawtool\n"

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "rawtool",
		Format:    "raw",
		Asset:     "rawtool",
		Files:     []PackageFile{{Name: "rawtool"}},
		Checksum:  &Checksum{Type: "github_release", Asset: "checksums.txt", Algorithm: "sha256"},
	}

	registry := urlRegistry(map[string][]byte{
		"rawtool":       binaryContent,
		"checksums.txt": []byte(manifest),
	})

	_, err := registry.Install(t.Context(), pkg, "v1.0.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")

	// Nothing should have been linked into BinDir on a failed verification.
	_, statErr := os.Stat(filepath.Join(BinDir(), executableName("rawtool")))
	assert.True(t, os.IsNotExist(statErr), "binary must not be installed on checksum failure")
}

func TestInstallE2E_ChecksumDisabled(t *testing.T) {
	// A package whose checksum is explicitly disabled installs without
	// requiring a checksum file to be present.
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := []byte("#!/bin/sh\necho nocheck")

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "rawtool",
		Format:    "raw",
		Asset:     "rawtool",
		Files:     []PackageFile{{Name: "rawtool"}},
		Checksum:  &Checksum{Type: "github_release", Asset: "checksums.txt", Enabled: new(false)},
	}

	registry := urlRegistry(map[string][]byte{
		"rawtool": binaryContent,
	})

	result, err := registry.Install(t.Context(), pkg, "v1.0.0")
	require.NoError(t, err)
	assertInstalledBinary(t, result, string(binaryContent), "rawtool")
}

func TestInstallE2E_ChecksumFileMissing_FailsClosed(t *testing.T) {
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := []byte("#!/bin/sh\necho x")

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "rawtool",
		Format:    "raw",
		Asset:     "rawtool",
		Files:     []PackageFile{{Name: "rawtool"}},
		Checksum:  &Checksum{Type: "github_release", Asset: "checksums.txt", Algorithm: "sha256"},
	}

	// Checksum file not served -> 404.
	registry := urlRegistry(map[string][]byte{
		"rawtool": binaryContent,
	})

	_, err := registry.Install(t.Context(), pkg, "v1.0.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "downloading checksum file")
}

func TestInstallE2E_UnsupportedChecksumType_Skips(t *testing.T) {
	// Unsupported checksum types are skipped (not enforced) so installs that
	// the registry can't help us verify still succeed.
	t.Setenv("DOCKER_AGENT_TOOLS_DIR", t.TempDir())

	binaryContent := []byte("#!/bin/sh\necho skip")

	pkg := &Package{
		RepoOwner: "acme",
		RepoName:  "rawtool",
		Format:    "raw",
		Asset:     "rawtool",
		Files:     []PackageFile{{Name: "rawtool"}},
		Checksum:  &Checksum{Type: "http", URL: "https://example.com/sum", Algorithm: "sha256"},
	}

	registry := urlRegistry(map[string][]byte{
		"rawtool": binaryContent,
	})

	result, err := registry.Install(t.Context(), pkg, "v1.0.0")
	require.NoError(t, err)
	assertInstalledBinary(t, result, string(binaryContent), "rawtool")
}
