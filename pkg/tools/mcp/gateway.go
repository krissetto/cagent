package mcp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/gateway"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

const (
	// Temp file names embed the creating PID (<prefix><pid>-<random>) so
	// the sweep can spare files of the current process. Older releases
	// used <prefix><random>; those legacy names are still swept by age.
	secretsFilePrefix = "mcp-secrets-"
	configFilePrefix  = "mcp-config-"

	// staleTempFileAge is the sweep's grace period for files of other
	// processes. It is a margin, not a guarantee: PID reuse or another
	// process outliving it can still be misjudged.
	staleTempFileAge = 24 * time.Hour
)

type GatewayToolset struct {
	*Toolset

	cleanUp func() error
}

var _ tools.ToolSet = (*GatewayToolset)(nil)

// NewGatewayToolset creates a new MCP toolset backed by the MCP gateway CLI
// for a cataloged server (a `ref:`-based toolset).
//
// The optional policy lets callers tune restart/backoff/timeout behaviour;
// see NewToolsetCommand for the semantics. When omitted, the zero value
// is used, which is behaviorally equivalent to lifecycle.PolicyFromConfig
// with a nil config (the resilient-profile default).
func NewGatewayToolset(ctx context.Context, name, mcpServerName string, secrets []gateway.Secret, config any, envProvider environment.Provider, cwd string, policy ...lifecycle.Policy) (*GatewayToolset, error) {
	slog.DebugContext(ctx, "Creating MCP Gateway toolset", "name", mcpServerName)

	// A crash or SIGKILL skips Stop and leaves plaintext secrets behind;
	// sweep those leftovers before writing new files.
	runStartupSweep(ctx)

	// Make sure all the required secrets are available in the environment.
	// TODO(dga): Ideally, the MCP gateway would use the same provider that we have.
	fileSecrets, err := writeSecretsToFile(ctx, mcpServerName, secrets, envProvider)
	if err != nil {
		return nil, fmt.Errorf("writing secrets to file: %w", err)
	}

	fileConfig, err := writeConfigToFile(ctx, mcpServerName, config)
	if err != nil {
		if rmErr := removeIfExists(fileSecrets); rmErr != nil {
			slog.WarnContext(ctx, "Failed to remove secrets file after config error", "error", rmErr, "path", fileSecrets)
		}
		return nil, fmt.Errorf("writing config to file: %w", err)
	}

	// Isolate ourselves from the MCP Toolkit config by always using the Docker MCP catalog and custom config and secrets.
	// This improves shareability of agents.
	// The gateway CLI only accepts file paths for --secrets/--config (stdin
	// already carries the MCP transport), so temp files are necessary.
	args := []string{
		"mcp", "gateway", "run",
		"--servers", mcpServerName,
		"--catalog", gateway.DockerCatalogURL,
		"--secrets", fileSecrets,
		"--config", fileConfig,
	}

	inner := NewToolsetCommand(name, "docker", args, nil, cwd, policy...)
	inner.description = "mcp(ref=" + mcpServerName + ")"

	return &GatewayToolset{
		Toolset: inner,
		cleanUp: func() error {
			return errors.Join(removeIfExists(fileSecrets), removeIfExists(fileConfig))
		},
	}, nil
}

func (t *GatewayToolset) Stop(ctx context.Context) error {
	stopErr := t.Toolset.Stop(ctx)

	cleanUpErr := t.cleanUp()
	if cleanUpErr != nil {
		slog.WarnContext(ctx, "Failed to clean up MCP Gateway temp files", "error", cleanUpErr)
	}

	return errors.Join(stopErr, cleanUpErr)
}

// removeIfExists treats a missing file as success so cleanUp, and
// therefore Stop, stays idempotent.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func writeSecretsToFile(ctx context.Context, mcpServerName string, secrets []gateway.Secret, envProvider environment.Provider) (string, error) {
	var secretValues []string
	for _, secret := range secrets {
		v, found := envProvider.Get(ctx, secret.Env)
		if !found || v == "" {
			return "", errors.New("missing environment variable " + secret.Env + " required by MCP server " + mcpServerName)
		}

		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("secret %s contains newline characters", secret.Env)
		}

		secretValues = append(secretValues, fmt.Sprintf("%s=%s", secret.Name, v))
	}

	// We have all the secrets, let's create a file with all of them for the MCP Gateway
	return writeTempFile(tempFilePattern(secretsFilePrefix), []byte(strings.Join(secretValues, "\n")))
}

func writeConfigToFile(_ context.Context, mcpServerName string, config any) (string, error) {
	buf, err := yaml.Marshal(map[string]any{
		mcpServerName: config,
	})
	if err != nil {
		return "", err
	}

	return writeTempFile(tempFilePattern(configFilePrefix), buf)
}

// tempFilePattern embeds the current PID so the sweep can tell our files
// from those of other processes.
func tempFilePattern(prefix string) string {
	return prefix + strconv.Itoa(os.Getpid()) + "-*"
}

// parseGatewayTempName matches gateway temp file names strictly:
// <prefix><pid>-<random> (current scheme) yields the pid digits,
// <prefix><random> (legacy) yields "". ok is false for any other name so
// lookalikes are never removed.
func parseGatewayTempName(name string) (pidPart string, ok bool) {
	rest, found := strings.CutPrefix(name, secretsFilePrefix)
	if !found {
		rest, found = strings.CutPrefix(name, configFilePrefix)
	}
	if !found {
		return "", false
	}

	pidPart, random, hasPID := strings.Cut(rest, "-")
	if !hasPID {
		return "", isDigits(rest)
	}
	if !isDigits(pidPart) || !isDigits(random) {
		return "", false
	}
	return pidPart, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// sweepStaleGatewayTempFiles removes regular files in dir whose names match
// a gateway temp scheme and whose mtime is older than maxAge, except files
// owned by currentPID, which are kept regardless of age. Symlinks and
// directories are skipped so nothing outside dir can be affected.
// Best-effort: per-entry errors don't stop the loop; the first is returned.
func sweepStaleGatewayTempFiles(dir string, currentPID int, now time.Time, maxAge time.Duration) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scanning temp dir: %w", err)
	}

	ownPIDPart := strconv.Itoa(currentPID)
	cutoff := now.Add(-maxAge)
	var firstErr error
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		pidPart, ours := parseGatewayTempName(e.Name())
		if !ours || pidPart == ownPIDPart {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

var startupSweepOnce sync.Once

// runStartupSweep swallows the sweep error so an unreadable temp dir does
// not block the toolset from being created.
func runStartupSweep(ctx context.Context) {
	startupSweepOnce.Do(func() {
		tempDir := os.TempDir()
		if err := sweepStaleGatewayTempFiles(tempDir, os.Getpid(), time.Now(), staleTempFileAge); err != nil {
			slog.WarnContext(ctx, "Failed to sweep stale MCP Gateway temp files", "error", err, "dir", tempDir)
		}
	})
}

func writeTempFile(nameTemplate string, content []byte) (string, error) {
	f, err := os.CreateTemp("", nameTemplate)
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}

	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	return f.Name(), nil
}
