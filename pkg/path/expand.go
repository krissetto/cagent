package path

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// jsEnvRef matches the JS-template form `${env.VAR}` (optional surrounding
// whitespace). Shell-style callers (os.Expand / os.ExpandEnv) historically
// only understood `${VAR}`, so the JS form was passed through as a literal.
// NormalizeEnvRefs rewrites it to `${VAR}` so both syntaxes resolve
// identically.
//
// Only the plain `${env.VAR}` reference is recognized; richer JS expressions
// such as `${env.VAR || 'default'}` are left untouched (evaluating them would
// require the goja engine, which pkg/path cannot import).
var jsEnvRef = regexp.MustCompile(`\$\{\s*env\.([A-Za-z_][A-Za-z0-9_]*)\s*\}`)

// NormalizeEnvRefs rewrites the JS-template form `${env.VAR}` to the shell
// form `${VAR}`, so os.Expand-based callers also accept the JS-style syntax
// used elsewhere in the config (issue #2615). Richer JS expressions are left
// untouched; see jsEnvRef.
func NormalizeEnvRefs(s string) string {
	return jsEnvRef.ReplaceAllString(s, "${$1}")
}

// ExpandEnvRefs resolves only the plain `${env.VAR}` form (see jsEnvRef)
// against the OS environment, leaving every other `$`-shaped substring
// (including `$VAR` and `${VAR}`) untouched. Use it for fields whose values
// may legitimately contain literal `$` (e.g. env values forwarded to
// subprocesses), where a full os.Expand pass would mangle them (issue
// #2615). Unset variables expand to the empty string, matching the
// JS-template semantics; a debug log makes that observable since the
// resulting value is otherwise indistinguishable from a legitimate "".
func ExpandEnvRefs(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return jsEnvRef.ReplaceAllStringFunc(s, func(m string) string {
		name := jsEnvRef.FindStringSubmatch(m)[1]
		value, ok := os.LookupEnv(name)
		if !ok {
			slog.Debug("${env.X} reference resolves to an empty string: variable is unset",
				"variable", name,
				"see", "https://github.com/docker/docker-agent/issues/2615",
			)
		}
		return value
	})
}

// ExpandPath expands shell-like patterns in a file path:
//   - ~ or ~/ at the start is replaced with the user's home directory
//   - Environment variables like ${HOME} or $HOME are expanded
//   - The JS-template form ${env.HOME} is accepted as an alias for ${HOME}
func ExpandPath(p string) string {
	if p == "" {
		return p
	}

	// Normalize ${env.VAR} to ${VAR} so the JS-template syntax used elsewhere
	// in the config also works in path fields (issue #2615).
	p = NormalizeEnvRefs(p)

	// Expand environment variables
	p = os.ExpandEnv(p)
	if p == "" {
		return ""
	}

	if expanded, err := ExpandHomeDir(p); err == nil {
		p = expanded
	}

	return filepath.Clean(p)
}

// ExpandWorkingDir expands a working-directory field like ExpandPath, and
// warns when a non-empty value expands to empty (typically an unset
// variable, e.g. `working_dir: ${env.UNSET}`). Callers fall back to a
// default directory in that case, which would otherwise be a silent
// surprise for commands that mutate files (#2615).
func ExpandWorkingDir(field, p string) string {
	expanded := ExpandPath(p)
	if p != "" && expanded == "" {
		slog.Warn("working_dir expanded to an empty string; falling back to the default directory",
			"field", field,
			"value", p,
			"see", "https://github.com/docker/docker-agent/issues/2615",
		)
	}
	return expanded
}
