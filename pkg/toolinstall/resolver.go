package toolinstall

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/fatih/color"
	"github.com/mattn/go-isatty"
	"golang.org/x/sync/singleflight"
)

// EnsureCommand makes sure a command binary is available.
// It checks PATH first, then the docker agent tools directory, then
// attempts to install from the aqua registry if auto-install is enabled.
//
// Returns the resolved command (may be the same string if found in PATH,
// or a full path to the installed binary) and any error encountered.
// When auto-install is disabled (globally or per-toolset), the original
// command is returned with no error.
func EnsureCommand(ctx context.Context, command, version string) (string, error) {
	if strings.EqualFold(os.Getenv("DOCKER_AGENT_AUTO_INSTALL"), "false") {
		return command, nil
	}

	lower := strings.ToLower(strings.TrimSpace(version))
	if lower == "false" || lower == "off" {
		return command, nil
	}

	resolvedPath, err := resolve(ctx, command, version, doInstall)
	if err != nil {
		return "", fmt.Errorf("auto-installing command %q: %w", command, err)
	}

	return resolvedPath, nil
}

// installGroup deduplicates concurrent installations of the same command.
// If two goroutines call resolve("fzf") simultaneously, only one performs
// the actual download and install; the other waits and receives the same result.
var installGroup singleflight.Group

// installFunc performs the actual package resolution and installation of a
// command. doInstall is the production implementation; tests pass their own
// (e.g. a panicking stub) so they never mutate package-level state.
type installFunc func(ctx context.Context, command, version string) (string, error)

// resolve checks if a command is available and installs it via install if
// needed. Returns the path to the usable binary.
func resolve(ctx context.Context, command, version string, install installFunc) (string, error) {
	// Check system PATH first — return original command name (not full path)
	// so the caller uses it as-is via exec.Command.
	if _, err := exec.LookPath(command); err == nil {
		return command, nil
	}

	// Check if already installed in our bin dir.
	binPath := filepath.Join(BinDir(), executableName(command))
	if info, err := os.Stat(binPath); err == nil && isExecutable(info) {
		return binPath, nil
	}

	// Use singleflight to deduplicate concurrent installs of the same command.
	result, err, _ := installGroup.Do(command, func() (any, error) {
		return safeInstall(ctx, command, version, install)
	})
	if err != nil {
		return "", err
	}

	return result.(string), nil
}

// safeInstall wraps install with panic recovery. Without this,
// singleflight wraps any panic in *panicError and re-raises it via
// `go panic(...)` (see golang.org/x/sync/singleflight), which is
// unrecoverable by callers and crashes the process. Issue #2765.
func safeInstall(ctx context.Context, command, version string, install installFunc) (path string, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "Panic during tool auto-install",
				"command", command, "panic", r, "stack", string(debug.Stack()))
			path = ""
			err = fmt.Errorf("auto-install for %q panicked: %v", command, r)
		}
	}()
	return install(ctx, command, version)
}

// doInstall performs the actual package resolution and installation.
func doInstall(ctx context.Context, command, versionRef string) (string, error) {
	// Re-check bin dir under singleflight — another goroutine may have
	// just finished installing while we were waiting.
	binPath := filepath.Join(BinDir(), executableName(command))
	if info, err := os.Stat(binPath); err == nil && isExecutable(info) {
		return binPath, nil
	}

	registry := SharedRegistry()

	pkg, version, err := lookupPackage(ctx, registry, command, versionRef)
	if err != nil {
		return "", err
	}

	if version == "" {
		version, err = resolveVersion(ctx, registry, pkg)
		if err != nil {
			return "", fmt.Errorf("resolving latest version for %s/%s: %w", pkg.RepoOwner, pkg.RepoName, err)
		}
	}

	pkgName := pkg.RepoOwner + "/" + pkg.RepoName
	slog.InfoContext(ctx, "Auto-installing missing command",
		"command", command, "package", pkgName, "version", version)
	announceInstall(command, pkgName, version)

	binaryPath, err := registry.Install(ctx, pkg, version)
	if err != nil {
		return "", fmt.Errorf("installing %s@%s: %w", pkgName, version, err)
	}

	slog.InfoContext(ctx, "Successfully installed command",
		"command", command, "package", fmt.Sprintf("%s@%s", pkgName, version), "path", binaryPath)

	return binaryPath, nil
}

// announceInstall prints a single user-visible line to stderr right
// before downloading a tool, so the user understands what the
// upcoming `go install` / GitHub-release chatter is about. We
// intentionally avoid stdout so this never gets piped into the
// agent's prompt or programmatic output.
//
// fatih/color's global NoColor is computed from stdout, so we cannot
// rely on it: when stderr is redirected (e.g. `agent run ... 2>log`)
// stdout may still be a TTY and emit escapes into the log, and when
// stdout is piped (e.g. `agent run ... | tee log`) but stderr is a
// TTY, NoColor is true and the styled branch silently degrades to
// plain text. Decide based on stderr explicitly and force colour on
// the local *color.Color values; honour NO_COLOR / TERM=dumb.
func announceInstall(command, pkgName, version string) {
	if stderrSupportsColor() {
		// fatih/color's package-level NoColor is set from stdout's TTY
		// state, so when stdout is piped but stderr is still a TTY the
		// SprintFunc helpers would silently strip ANSI codes. Force the
		// local colours on after we've decided stderr can handle them.
		bold := color.New(color.Bold)
		bold.EnableColor()
		faint := color.New(color.Faint)
		faint.EnableColor()
		fmt.Fprintf(os.Stderr, "Installing %s %s\n",
			bold.Sprint(command), faint.Sprintf("(%s@%s)", pkgName, version))
		return
	}
	fmt.Fprintf(os.Stderr, "Installing %s (%s@%s)\n", command, pkgName, version)
}

// stderrSupportsColor reports whether ANSI escapes are safe to write
// to os.Stderr.
func stderrSupportsColor() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if os.Stderr == nil {
		return false
	}
	fd := os.Stderr.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// lookupPackage resolves the aqua package for a command.
// If versionRef is provided (e.g. "owner/repo@v1.0"), it parses the reference
// and looks up by name. Otherwise, it searches by command name.
// Returns the package, the explicit version (if any), and any error.
func lookupPackage(ctx context.Context, registry *Registry, command, versionRef string) (*Package, string, error) {
	if versionRef == "" {
		pkg, err := registry.LookupByCommand(ctx, command)
		if err != nil {
			return nil, "", fmt.Errorf("looking up command %q in aqua registry: %w", command, err)
		}
		return pkg, "", nil
	}

	owner, repo, version, err := parseAquaRef(versionRef)
	if err != nil {
		return nil, "", fmt.Errorf("parsing aqua reference: %w", err)
	}

	pkg, err := registry.LookupByName(ctx, owner+"/"+repo)
	if err != nil {
		return nil, "", fmt.Errorf("looking up aqua package %s/%s: %w", owner, repo, err)
	}

	return pkg, version, nil
}

// resolveVersion determines the latest version for a package.
func resolveVersion(ctx context.Context, registry *Registry, pkg *Package) (string, error) {
	// Check for a version_filter with a "startsWith" prefix (multi-module repos).
	if prefix := extractVersionPrefix(pkg.VersionFilter); prefix != "" {
		return registry.latestVersionFiltered(ctx, pkg.RepoOwner, pkg.RepoName, prefix)
	}

	// For go types, use "latest" and let go install resolve it.
	if pkg.IsGoPackage() {
		return "latest", nil
	}

	return registry.latestVersion(ctx, pkg.RepoOwner, pkg.RepoName)
}

// extractVersionPrefix parses an aqua version_filter expression like
// 'Version startsWith "gopls/"' and returns the prefix string.
// Returns "" if the filter doesn't match this pattern.
func extractVersionPrefix(filter string) string {
	filter = strings.TrimSpace(filter)
	const marker = "startsWith"
	_, after, ok := strings.Cut(filter, marker)
	if !ok {
		return ""
	}

	rest := strings.TrimSpace(after)
	if len(rest) >= 2 && (rest[0] == '"' || rest[0] == '\'') {
		quote := rest[0]
		end := strings.IndexByte(rest[1:], quote)
		if end >= 0 {
			return rest[1 : end+1]
		}
	}

	return ""
}

// parseAquaRef parses an aqua reference string into owner, repo, and version.
// Format: "owner/repo" or "owner/repo@version"
func parseAquaRef(ref string) (owner, repo, version string, err error) {
	ref = strings.TrimSpace(ref)

	atParts := strings.SplitN(ref, "@", 2)
	namePart := atParts[0]
	if len(atParts) == 2 {
		version = atParts[1]
	}

	parts := strings.SplitN(namePart, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("invalid aqua reference %q: expected owner/repo[@version] format", ref)
	}

	return parts[0], parts[1], version, nil
}
