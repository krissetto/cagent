package sandbox_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/sandbox"
)

// loginKitSpec mirrors the subset of the sbx kit schema the generated
// spec must produce; parsing (rather than substring-matching) the YAML
// catches host values that would alter the document structure.
type loginKitSpec struct {
	SchemaVersion string `yaml:"schemaVersion"`
	Kind          string `yaml:"kind"`
	Name          string `yaml:"name"`
	Credentials   []struct {
		Service string `yaml:"service"`
		APIKey  struct {
			Name         string `yaml:"name"`
			ProxyManaged bool   `yaml:"proxyManaged"`
			Inject       []struct {
				Domain string `yaml:"domain"`
				Header string `yaml:"header"`
				Format string `yaml:"format"`
			} `yaml:"inject"`
		} `yaml:"apiKey"`
	} `yaml:"credentials"`
}

func TestLoginKit_DockerGateway(t *testing.T) {
	cacheDir := t.TempDir()
	paths.SetCacheDir(cacheDir)
	t.Cleanup(func() { paths.SetCacheDir("") })

	dir, err := sandbox.LoginKit("https://api.docker.com/v1/gateway?x=1")
	require.NoError(t, err)
	require.NotEmpty(t, dir)
	assert.Equal(t, filepath.Join(cacheDir, "sandbox-login-kit", "api.docker.com"), dir)

	data, err := os.ReadFile(filepath.Join(dir, "spec.yaml"))
	require.NoError(t, err)

	var spec loginKitSpec
	require.NoError(t, yaml.Unmarshal(data, &spec))
	assert.Equal(t, "2", spec.SchemaVersion)
	assert.Equal(t, "mixin", spec.Kind)
	assert.Equal(t, "docker-agent-gateway-auth", spec.Name)
	require.Len(t, spec.Credentials, 1)
	cred := spec.Credentials[0]
	assert.Equal(t, "sbx-login", cred.Service)
	assert.Equal(t, "DOCKER_TOKEN", cred.APIKey.Name)
	assert.True(t, cred.APIKey.ProxyManaged)
	require.Len(t, cred.APIKey.Inject, 1)
	assert.Equal(t, "api.docker.com", cred.APIKey.Inject[0].Domain)
	assert.Equal(t, "Authorization", cred.APIKey.Inject[0].Header)
	assert.Equal(t, "Bearer %s", cred.APIKey.Inject[0].Format)
}

func TestLoginKit_HostIsLowercasedAndPortStripped(t *testing.T) {
	cacheDir := t.TempDir()
	paths.SetCacheDir(cacheDir)
	t.Cleanup(func() { paths.SetCacheDir("") })

	dir, err := sandbox.LoginKit("https://Gateway.Docker.com:443/proxy")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cacheDir, "sandbox-login-kit", "gateway.docker.com"), dir)
}

func TestLoginKit_RejectedGateways(t *testing.T) {
	t.Parallel()

	// The sandbox proxy only ever injects the login token into
	// docker.com hosts over HTTPS, and the hostname is interpolated
	// into YAML / used as a directory name — anything else must not
	// produce a kit.
	for _, gateway := range []string{
		"",
		"https://models.example.com",
		"http://api.docker.com",           // plaintext
		"https://notdocker.com",           // suffix trickery
		"https://docker.com.evil.example", // prefix trickery
		"http://localhost:8080",
		"::bogus::",
		// url.Parse accepts these, but the hosts are not conventional
		// DNS names and would alter the generated YAML structure.
		"https://!x.docker.com",
		"https://&x.docker.com",
		"https://*x.docker.com",
		"https://-x.docker.com",
	} {
		dir, err := sandbox.LoginKit(gateway)
		require.NoError(t, err, "gateway %q", gateway)
		assert.Empty(t, dir, "gateway %q must not produce a login kit", gateway)
	}
}
