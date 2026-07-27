package root

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessionplan"
)

// newPlansTestService builds a hermetic plans.Service over temp directories,
// returning both so tests can plant files directly. No test ever touches the
// real user data directory.
func newPlansTestService(t *testing.T) (svc plans.Service, sharedDir, sessionDir string) {
	t.Helper()
	sharedDir = t.TempDir()
	sessionDir = t.TempDir()
	svc = plans.NewService(plan.NewFilesystemStorage(sharedDir), plans.WithSessionDir(sessionDir))
	return svc, sharedDir, sessionDir
}

func executePlansIn(t *testing.T, svc plans.Service, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newPlansCmd(withPlansService(svc))
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(stdin)
	cmd.SetArgs(args)
	cmd.SetContext(t.Context())
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func executePlans(t *testing.T, svc plans.Service, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return executePlansIn(t, svc, strings.NewReader(""), args...)
}

func mustCreatePlan(t *testing.T, svc plans.Service, name, content string) plans.Plan {
	t.Helper()
	p, err := svc.Create(t.Context(), plans.CreateRequest{Ref: plans.SharedRef(name), Content: content})
	require.NoError(t, err)
	return p
}

func mustGetPlan(t *testing.T, svc plans.Service, ref plans.Ref) plans.Plan {
	t.Helper()
	p, err := svc.Get(t.Context(), ref)
	require.NoError(t, err)
	return p
}

func writePlanContentFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "content.md")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func writeSessionPlanFile(t *testing.T, dir, sessionID, content string) string {
	t.Helper()
	path, err := sessionplan.WriteContent(dir, sessionID, content)
	require.NoError(t, err)
	return path
}

func requirePlansStatusCode(t *testing.T, err error, want int) {
	t.Helper()
	require.Error(t, err)
	statusErr, ok := errors.AsType[cli.StatusError](err)
	require.True(t, ok, "expected a cli.StatusError, got %T", err)
	assert.Equal(t, want, statusErr.StatusCode)
}

type plansTestError struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	Scope           string `json:"scope"`
	Name            string `json:"name"`
	Op              string `json:"op"`
	ExpectedVersion *int   `json:"expected_version"`
	CurrentVersion  *int   `json:"current_version"`
}

// decodePlansError asserts stderr is exactly one JSON object honouring the
// error contract and returns its "error" body.
func decodePlansError(t *testing.T, stderr string) plansTestError {
	t.Helper()
	assert.Equal(t, 1, strings.Count(stderr, "\n"), "stderr must be a single JSON line: %q", stderr)
	var doc struct {
		SchemaVersion string         `json:"schema_version"`
		Error         plansTestError `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(stderr), &doc), "stderr must be JSON: %q", stderr)
	assert.Equal(t, "1", doc.SchemaVersion)
	require.NotEmpty(t, doc.Error.Code)
	require.NotEmpty(t, doc.Error.Message)
	return doc.Error
}

type plansTestListDocument struct {
	SchemaVersion string       `json:"schema_version"`
	Plans         []plans.Plan `json:"plans"`
	Warnings      []string     `json:"warnings"`
}

type plansTestPlanDocument struct {
	SchemaVersion string     `json:"schema_version"`
	Plan          plans.Plan `json:"plan"`
}

// --- Registration --------------------------------------------------------------

func TestPlansCommand_RegisteredOnRoot(t *testing.T) {
	t.Parallel()

	cmd, _, err := NewRootCmd().Find([]string{"plans"})
	require.NoError(t, err)
	require.Equal(t, "plans", cmd.Name())

	var subs []string
	for _, sub := range cmd.Commands() {
		subs = append(subs, sub.Name())
	}
	for _, want := range []string{"list", "get", "create", "update", "status", "export", "delete"} {
		assert.Contains(t, subs, want)
	}
}

// --- List ----------------------------------------------------------------------

func TestPlansList_EmptyJSON(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)

	stdout, stderr, err := executePlans(t, svc, "list", "--json")
	require.NoError(t, err)
	assert.Empty(t, stderr)

	var doc plansTestListDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, "1", doc.SchemaVersion)
	assert.Empty(t, doc.Plans)
	assert.Empty(t, doc.Warnings)
	// The empty listing must encode as [], never null.
	assert.Contains(t, stdout, `"plans": []`)
}

func TestPlansList_HumanShowsMetadataAndSendsWarningsToStderr(t *testing.T) {
	t.Parallel()
	svc, sharedDir, sessionDir := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{
		Ref: plans.SharedRef("alpha"), Content: "body", Title: "Alpha plan", Status: "draft",
	})
	require.NoError(t, err)
	writeSessionPlanFile(t, sessionDir, "sess-1", "# session plan")
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "bad.json"), []byte("{nope"), 0o600))

	stdout, stderr, err := executePlans(t, svc, "list", "--session", "sess-1")
	require.NoError(t, err)

	assert.Regexp(t, `SCOPE\s+NAME\s+STATUS\s+VERSION\s+UPDATED\s+TITLE`, stdout)
	assert.Regexp(t, `shared\s+alpha\s+draft\s+1\s+\S+\s+Alpha plan`, stdout)
	// Session plans have no version or status: shown as "-".
	assert.Regexp(t, `session\s+sess-1\s+-\s+-\s+\S+\s+-`, stdout)
	assert.NotContains(t, stdout, "\x1b[", "human output must be ANSI-free")

	// Human-mode warnings go to stderr, not stdout.
	assert.Contains(t, stderr, "Warning:")
	assert.Contains(t, stderr, "bad")
	assert.NotContains(t, stdout, "Warning")
}

func TestPlansList_JSONMetadataAndWarnings(t *testing.T) {
	t.Parallel()
	svc, sharedDir, sessionDir := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{
		Ref: plans.SharedRef("alpha"), Content: "body", Title: "Alpha plan", Author: "alice", Status: "draft",
	})
	require.NoError(t, err)
	writeSessionPlanFile(t, sessionDir, "sess-1", "# session plan")
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "bad.json"), []byte("{nope"), 0o600))

	stdout, stderr, err := executePlans(t, svc, "list", "--session", "sess-1", "--json")
	require.NoError(t, err)
	assert.Empty(t, stderr, "JSON mode must not write warnings to stderr")

	var doc plansTestListDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	require.Len(t, doc.Plans, 2)

	sess := doc.Plans[0]
	assert.Equal(t, plans.ScopeSession, sess.Scope)
	assert.Equal(t, "sess-1", sess.Name)
	assert.Equal(t, "sess-1", sess.SessionID)
	assert.Nil(t, sess.Version)
	assert.Empty(t, sess.Content, "list is metadata only")

	shared := doc.Plans[1]
	assert.Equal(t, plans.ScopeShared, shared.Scope)
	assert.Equal(t, "alpha", shared.Name)
	assert.Equal(t, "Alpha plan", shared.Title)
	assert.Equal(t, "alice", shared.Author)
	assert.Equal(t, "draft", shared.Status)
	require.NotNil(t, shared.Version)
	assert.Equal(t, 1, *shared.Version)

	require.Len(t, doc.Warnings, 1)
	assert.Contains(t, doc.Warnings[0], "bad")
}

func TestPlansList_InvalidSessionID(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)

	stdout, stderr, err := executePlans(t, svc, "list", "--session", "../escape", "--json")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
}

// --- Get -----------------------------------------------------------------------

func TestPlansGet_SharedHuman(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{
		Ref: plans.SharedRef("release"), Content: "# release\n\nstep 1\n", Title: "Release", Status: "draft",
	})
	require.NoError(t, err)

	stdout, stderr, err := executePlans(t, svc, "get", "release")
	require.NoError(t, err)
	// Content alone on stdout; metadata stays visible on stderr.
	assert.Equal(t, "# release\n\nstep 1\n", stdout)
	assert.Contains(t, stderr, `shared plan "release"`)
	assert.Contains(t, stderr, "title: Release")
	assert.Contains(t, stderr, "status: draft")
	assert.Contains(t, stderr, "version: 1")
}

func TestPlansGet_SharedJSON(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{
		Ref: plans.SharedRef("release"), Content: "the body", Title: "Release", Author: "alice", Status: "draft",
	})
	require.NoError(t, err)

	stdout, stderr, err := executePlans(t, svc, "get", "release", "--json")
	require.NoError(t, err)
	assert.Empty(t, stderr, "JSON mode keeps metadata in the document, not on stderr")

	var doc plansTestPlanDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, "1", doc.SchemaVersion)
	assert.Equal(t, plans.ScopeShared, doc.Plan.Scope)
	assert.Equal(t, "release", doc.Plan.Name)
	assert.Equal(t, "the body", doc.Plan.Content)
	assert.Equal(t, "Release", doc.Plan.Title)
	assert.Equal(t, "alice", doc.Plan.Author)
	assert.Equal(t, "draft", doc.Plan.Status)
	require.NotNil(t, doc.Plan.Version)
	assert.Equal(t, 1, *doc.Plan.Version)
	assert.False(t, doc.Plan.UpdatedAt.IsZero())
}

func TestPlansGet_Session(t *testing.T) {
	t.Parallel()
	svc, _, sessionDir := newPlansTestService(t)
	path := writeSessionPlanFile(t, sessionDir, "sess-1", "# session plan\n")

	// --session alone implies session scope; --scope session spells it out.
	for _, args := range [][]string{
		{"get", "--session", "sess-1"},
		{"get", "--scope", "session", "--session", "sess-1"},
	} {
		stdout, stderr, err := executePlans(t, svc, args...)
		require.NoError(t, err, "args %v", args)
		assert.Equal(t, "# session plan\n", stdout)
		assert.Contains(t, stderr, `session plan "sess-1"`)
		assert.Contains(t, stderr, "version: -", "session plans have no version")
	}

	stdout, _, err := executePlans(t, svc, "get", "--session", "sess-1", "--json")
	require.NoError(t, err)
	var doc plansTestPlanDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, plans.ScopeSession, doc.Plan.Scope)
	assert.Equal(t, "sess-1", doc.Plan.SessionID)
	assert.Equal(t, "# session plan\n", doc.Plan.Content)
	assert.Equal(t, path, doc.Plan.Path)
	assert.Nil(t, doc.Plan.Version)
}

func TestPlansGet_RefValidation(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)

	tests := []struct {
		args    []string
		wantMsg string
	}{
		{[]string{"get"}, "a plan name is required"},
		{[]string{"get", "p", "--session", "sess-1"}, "addressed by --session"},
		{[]string{"get", "--scope", "session"}, "requires --session"},
		{[]string{"get", "--scope", "shared", "--session", "sess-1"}, "--session selects a session plan"},
		{[]string{"get", "p", "--scope", "bogus"}, "invalid --scope"},
	}
	for _, tt := range tests {
		stdout, stderr, err := executePlans(t, svc, append(tt.args, "--json")...)
		requirePlansStatusCode(t, err, 1)
		assert.Empty(t, stdout, "args %v", tt.args)
		body := decodePlansError(t, stderr)
		assert.Equal(t, "invalid_argument", body.Code, "args %v", tt.args)
		assert.Contains(t, body.Message, tt.wantMsg, "args %v", tt.args)
	}
}

func TestPlansGet_NotFoundJSON(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)

	stdout, stderr, err := executePlans(t, svc, "get", "missing", "--json")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "not_found", body.Code)
	assert.Equal(t, "shared", body.Scope)
	assert.Equal(t, "missing", body.Name)
}

func TestPlansGet_NotFoundHuman(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)

	stdout, stderr, err := executePlans(t, svc, "get", "missing")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, `shared plan "missing" not found`)
	// The error is rendered exactly once, by the command itself.
	assert.Equal(t, 1, strings.Count(stderr, "not found"))
}

func TestPlansGet_InvalidName(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)

	_, stderr, err := executePlans(t, svc, "get", "UPPER", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "invalid plan name")
}

func TestPlansGet_Corrupt(t *testing.T) {
	t.Parallel()
	svc, sharedDir, _ := newPlansTestService(t)
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "broken.json"), []byte("{not json"), 0o600))

	_, stderr, err := executePlans(t, svc, "get", "broken", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "corrupt", body.Code, "a corrupt plan must never read as missing")
	assert.Equal(t, "shared", body.Scope)
	assert.Equal(t, "broken", body.Name)
}

// --- Create --------------------------------------------------------------------

func TestPlansCreate_FromFile(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	file := writePlanContentFile(t, "plan body")

	stdout, stderr, err := executePlans(t, svc,
		"create", "release", "--file", file, "--title", "Release", "--author", "alice", "--status", "draft")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "Created shared plan \"release\" (version 1)\n", stdout)

	p := mustGetPlan(t, svc, plans.SharedRef("release"))
	assert.Equal(t, "plan body", p.Content)
	assert.Equal(t, "Release", p.Title)
	assert.Equal(t, "alice", p.Author)
	assert.Equal(t, "draft", p.Status)
}

func TestPlansCreate_FromStdin(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)

	// Headless: content is piped through a non-TTY stdin via --file -.
	stdout, _, err := executePlansIn(t, svc, strings.NewReader("piped body"), "create", "p", "--file", "-", "--json")
	require.NoError(t, err)

	var doc plansTestPlanDocument
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, "1", doc.SchemaVersion)
	assert.Equal(t, "piped body", doc.Plan.Content)
	require.NotNil(t, doc.Plan.Version)
	assert.Equal(t, 1, *doc.Plan.Version)
}

// stdinMustNotBeRead fails the test when a command reads stdin without being
// asked to via --file -.
type stdinMustNotBeRead struct{ t *testing.T }

func (r stdinMustNotBeRead) Read([]byte) (int, error) {
	r.t.Error("stdin must not be read when --file is not given")
	return 0, io.EOF
}

func TestPlansCreate_RequiresFileWithoutPrompting(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)

	// Without --file the command must fail immediately instead of waiting on
	// an interactive terminal for content.
	_, _, err := executePlansIn(t, svc, stdinMustNotBeRead{t: t}, "create", "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"file" not set`)
}

func TestPlansCreate_ExistingNameConflicts(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "original")
	file := writePlanContentFile(t, "clobber")

	stdout, stderr, err := executePlans(t, svc, "create", "p", "--file", file, "--json")
	requirePlansStatusCode(t, err, plansConflictExitCode)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "conflict", body.Code)
	assert.Equal(t, "p", body.Name)
	require.NotNil(t, body.CurrentVersion)
	assert.Equal(t, 1, *body.CurrentVersion)

	p := mustGetPlan(t, svc, plans.SharedRef("p"))
	assert.Equal(t, "original", p.Content, "a conflicting create must not overwrite")
}

func TestPlansCreate_Validation(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)

	// Empty content is rejected by the service.
	empty := writePlanContentFile(t, "")
	_, stderr, err := executePlans(t, svc, "create", "p", "--file", empty, "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "content must not be empty")

	// An unreadable content file is a clear input error.
	_, stderr, err = executePlans(t, svc, "create", "p", "--file", filepath.Join(t.TempDir(), "missing.md"), "--json")
	requirePlansStatusCode(t, err, 1)
	body = decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "reading plan content")

	// Invalid plan names are rejected by the service's canonical rule.
	file := writePlanContentFile(t, "x")
	_, stderr, err = executePlans(t, svc, "create", "../escape", "--file", file, "--json")
	requirePlansStatusCode(t, err, 1)
	body = decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "invalid plan name")
}

// --- Update --------------------------------------------------------------------

func TestPlansUpdate_Success(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{
		Ref: plans.SharedRef("p"), Content: "v1", Title: "Original", Status: "draft",
	})
	require.NoError(t, err)
	file := writePlanContentFile(t, "v2")

	stdout, _, err := executePlans(t, svc, "update", "p", "--file", file, "--expected-version", "1", "--author", "bob")
	require.NoError(t, err)
	assert.Equal(t, "Updated shared plan \"p\" (version 2)\n", stdout)

	p := mustGetPlan(t, svc, plans.SharedRef("p"))
	assert.Equal(t, "v2", p.Content)
	assert.Equal(t, "Original", p.Title, "omitted metadata flags preserve the previous value")
	assert.Equal(t, "bob", p.Author)
	assert.Equal(t, "draft", p.Status)

	// An explicitly empty metadata flag overwrites rather than preserves.
	stdout, _, err = executePlans(t, svc, "update", "p", "--file", file, "--expected-version", "2", "--title", "")
	require.NoError(t, err)
	assert.Contains(t, stdout, "version 3")
	assert.Empty(t, mustGetPlan(t, svc, plans.SharedRef("p")).Title)
}

func TestPlansUpdate_StaleConflict(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), plans.UpdateRequest{Ref: plans.SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)
	file := writePlanContentFile(t, "stale write")

	stdout, stderr, err := executePlans(t, svc, "update", "p", "--file", file, "--expected-version", "1", "--json")
	requirePlansStatusCode(t, err, plansConflictExitCode)
	assert.Empty(t, stdout, "JSON mode must keep stdout free of prose")

	// The typed conflict stays reachable through the returned error.
	conflictErr, ok := errors.AsType[*plans.ConflictError](err)
	require.True(t, ok, "the cli.StatusError must wrap the typed conflict")
	assert.Equal(t, 2, conflictErr.Current)

	body := decodePlansError(t, stderr)
	assert.Equal(t, "conflict", body.Code)
	assert.Equal(t, "shared", body.Scope)
	assert.Equal(t, "p", body.Name)
	require.NotNil(t, body.ExpectedVersion)
	assert.Equal(t, 1, *body.ExpectedVersion)
	require.NotNil(t, body.CurrentVersion)
	assert.Equal(t, 2, *body.CurrentVersion)

	p := mustGetPlan(t, svc, plans.SharedRef("p"))
	assert.Equal(t, "v2", p.Content, "the losing write must not touch the plan")
}

func TestPlansUpdate_Force(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), plans.UpdateRequest{Ref: plans.SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)
	file := writePlanContentFile(t, "forced")

	stdout, _, err := executePlans(t, svc, "update", "p", "--file", file, "--force")
	require.NoError(t, err)
	assert.Contains(t, stdout, "version 3")
	assert.Equal(t, "forced", mustGetPlan(t, svc, plans.SharedRef("p")).Content)
}

func TestPlansUpdate_RequiresGuard(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "v1")
	file := writePlanContentFile(t, "v2")

	// Neither --expected-version nor --force: refuse instead of passing a
	// nil expected version by accident.
	_, _, err := executePlans(t, svc, "update", "p", "--file", file)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one of the flags in the group [expected-version force] is required")
	assert.Equal(t, "v1", mustGetPlan(t, svc, plans.SharedRef("p")).Content)
}

func TestPlansUpdate_NotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	file := writePlanContentFile(t, "x")

	_, stderr, err := executePlans(t, svc, "update", "ghost", "--file", file, "--force", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "not_found", body.Code)
	assert.Equal(t, "ghost", body.Name)
}

func TestPlansMutations_ExpectedVersionMustBePositive(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "v1")
	file := writePlanContentFile(t, "v2")

	for _, args := range [][]string{
		{"update", "p", "--file", file, "--expected-version", "0"},
		{"status", "p", "done", "--expected-version", "-1"},
		{"delete", "p", "--expected-version", "0"},
	} {
		_, stderr, err := executePlans(t, svc, append(args, "--json")...)
		requirePlansStatusCode(t, err, 1)
		body := decodePlansError(t, stderr)
		assert.Equal(t, "invalid_argument", body.Code, "args %v", args)
		assert.Contains(t, body.Message, "--expected-version must be at least 1", "args %v", args)
	}
}

// --- Status --------------------------------------------------------------------

func TestPlansStatus_Success(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	stdout, _, err := executePlans(t, svc, "status", "p", "in-progress", "--expected-version", "1")
	require.NoError(t, err)
	assert.Equal(t, "Set status of shared plan \"p\" to \"in-progress\" (version 2)\n", stdout)

	p := mustGetPlan(t, svc, plans.SharedRef("p"))
	assert.Equal(t, "in-progress", p.Status)
	assert.Equal(t, "body", p.Content, "setting the status must preserve the body")
}

func TestPlansStatus_StaleConflict(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	_, err := svc.Create(t.Context(), plans.CreateRequest{Ref: plans.SharedRef("p"), Content: "body", Status: "draft"})
	require.NoError(t, err)
	_, err = svc.Update(t.Context(), plans.UpdateRequest{Ref: plans.SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	_, stderr, err := executePlans(t, svc, "status", "p", "done", "--expected-version", "1", "--json")
	requirePlansStatusCode(t, err, plansConflictExitCode)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "conflict", body.Code)
	require.NotNil(t, body.CurrentVersion)
	assert.Equal(t, 2, *body.CurrentVersion)

	assert.Equal(t, "draft", mustGetPlan(t, svc, plans.SharedRef("p")).Status, "the losing status write must not touch the plan")
}

func TestPlansStatus_Validation(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	// An empty status string is rejected by the service.
	_, stderr, err := executePlans(t, svc, "status", "p", "", "--force", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "status must not be empty")

	// A single positional in shared scope is missing either name or status.
	_, stderr, err = executePlans(t, svc, "status", "p", "--force", "--json")
	requirePlansStatusCode(t, err, 1)
	body = decodePlansError(t, stderr)
	assert.Equal(t, "invalid_argument", body.Code)
	assert.Contains(t, body.Message, "plans status <name> <status>")
}

// --- Session mutations are unsupported -------------------------------------------

func TestPlansMutations_SessionUnsupported(t *testing.T) {
	t.Parallel()
	svc, _, sessionDir := newPlansTestService(t)
	writeSessionPlanFile(t, sessionDir, "sess-1", "# plan")
	file := writePlanContentFile(t, "new content")

	tests := []struct {
		args   []string
		wantOp string
	}{
		{[]string{"update", "--session", "sess-1", "--file", file, "--force"}, "update"},
		{[]string{"status", "--session", "sess-1", "done", "--force"}, "set_status"},
		{[]string{"delete", "--session", "sess-1", "--force"}, "delete"},
	}
	for _, tt := range tests {
		stdout, stderr, err := executePlans(t, svc, append(tt.args, "--json")...)
		requirePlansStatusCode(t, err, 1)
		assert.Empty(t, stdout, "args %v", tt.args)
		body := decodePlansError(t, stderr)
		assert.Equal(t, "unsupported", body.Code, "args %v", tt.args)
		assert.Equal(t, "session", body.Scope, "args %v", tt.args)
		assert.Equal(t, tt.wantOp, body.Op, "args %v", tt.args)
		assert.Contains(t, body.Message, "within its session", "the error must tell the caller what to do instead")
	}

	// The refused mutations left the session plan untouched.
	p := mustGetPlan(t, svc, plans.SessionRef("sess-1"))
	assert.Equal(t, "# plan", p.Content)
}

// --- Export --------------------------------------------------------------------

func TestPlansExport_Shared(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "release", "the body")
	dest := filepath.Join(t.TempDir(), "nested", "plan.md")

	stdout, stderr, err := executePlans(t, svc, "export", "release", "--output", dest)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "Exported shared plan \"release\" (version 1) to "+dest+" (8 bytes)\n", stdout)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "the body", string(data))
}

func TestPlansExport_SessionJSON(t *testing.T) {
	t.Parallel()
	svc, _, sessionDir := newPlansTestService(t)
	writeSessionPlanFile(t, sessionDir, "sess-2", "# session plan")
	dest := filepath.Join(t.TempDir(), "plan.md")

	stdout, _, err := executePlans(t, svc, "export", "--session", "sess-2", "--output", dest, "--json")
	require.NoError(t, err)

	var doc struct {
		SchemaVersion string             `json:"schema_version"`
		Export        plans.ExportResult `json:"export"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, "1", doc.SchemaVersion)
	assert.Equal(t, plans.ScopeSession, doc.Export.Scope)
	assert.Equal(t, "sess-2", doc.Export.Name)
	assert.Equal(t, dest, doc.Export.Path)
	assert.Nil(t, doc.Export.Version, "session plans have no version to export")
	assert.Equal(t, len("# session plan"), doc.Export.BytesWritten)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "# session plan", string(data))
}

func TestPlansExport_NotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	dest := filepath.Join(t.TempDir(), "plan.md")

	_, stderr, err := executePlans(t, svc, "export", "ghost", "--output", dest, "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "not_found", body.Code)
	assert.NoFileExists(t, dest)
}

func TestPlansExport_RequiresOutput(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	_, _, err := executePlans(t, svc, "export", "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"output" not set`)
}

// --- Delete --------------------------------------------------------------------

func TestPlansDelete_WithExpectedVersion(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	stdout, _, err := executePlans(t, svc, "delete", "p", "--expected-version", "1")
	require.NoError(t, err)
	assert.Equal(t, "Deleted shared plan \"p\"\n", stdout)

	_, err = svc.Get(t.Context(), plans.SharedRef("p"))
	notFound, ok := errors.AsType[*plans.NotFoundError](err)
	require.True(t, ok, "expected a *plans.NotFoundError, got %v", err)
	assert.Equal(t, "p", notFound.Name)
}

func TestPlansDelete_JSON(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	stdout, _, err := executePlans(t, svc, "delete", "p", "--force", "--json")
	require.NoError(t, err)

	var doc struct {
		SchemaVersion string `json:"schema_version"`
		Deleted       struct {
			Scope string `json:"scope"`
			Name  string `json:"name"`
		} `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &doc))
	assert.Equal(t, "1", doc.SchemaVersion)
	assert.Equal(t, "shared", doc.Deleted.Scope)
	assert.Equal(t, "p", doc.Deleted.Name)
}

func TestPlansDelete_StaleConflictPreservesPlan(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), plans.UpdateRequest{Ref: plans.SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	_, stderr, err := executePlans(t, svc, "delete", "p", "--expected-version", "1", "--json")
	requirePlansStatusCode(t, err, plansConflictExitCode)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "conflict", body.Code)
	require.NotNil(t, body.CurrentVersion)
	assert.Equal(t, 2, *body.CurrentVersion)

	assert.Equal(t, "v2", mustGetPlan(t, svc, plans.SharedRef("p")).Content, "the conflicting delete must leave the plan in place")
}

func TestPlansDelete_SafetyRequiresGuardOrForce(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)
	mustCreatePlan(t, svc, "p", "body")

	// The CLI never prompts, so a bare delete is refused.
	_, _, err := executePlans(t, svc, "delete", "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one of the flags in the group [expected-version force] is required")

	// The guard and the deliberate opt-out are mutually exclusive.
	_, _, err = executePlans(t, svc, "delete", "p", "--expected-version", "1", "--force")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "none of the others can be")

	_, err = svc.Get(t.Context(), plans.SharedRef("p"))
	require.NoError(t, err, "the refused deletes must not remove the plan")
}

func TestPlansDelete_NotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newPlansTestService(t)

	_, stderr, err := executePlans(t, svc, "delete", "ghost", "--force", "--json")
	requirePlansStatusCode(t, err, 1)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "not_found", body.Code)
	assert.Equal(t, "ghost", body.Name)
}

// --- Storage failures -------------------------------------------------------------

// failingPlanStorage is a plan.Storage whose every method fails, to verify
// backend failures surface through the CLI as the storage error code.
type failingPlanStorage struct{ err error }

var _ plan.Storage = failingPlanStorage{}

func (f failingPlanStorage) Get(context.Context, string) (plan.Plan, bool, error) {
	return plan.Plan{}, false, f.err
}

func (f failingPlanStorage) Upsert(context.Context, plan.UpsertRequest) (plan.Plan, error) {
	return plan.Plan{}, f.err
}

func (f failingPlanStorage) List(context.Context) ([]plan.Summary, []string, error) {
	return nil, nil, f.err
}

func (f failingPlanStorage) Delete(context.Context, string, *int) (bool, error) {
	return false, f.err
}

func TestPlansList_StorageErrorJSON(t *testing.T) {
	t.Parallel()
	svc := plans.NewService(failingPlanStorage{err: errors.New("backend boom")}, plans.WithSessionDir(t.TempDir()))

	stdout, stderr, err := executePlans(t, svc, "list", "--json")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	body := decodePlansError(t, stderr)
	assert.Equal(t, "storage", body.Code)
	assert.Equal(t, "shared", body.Scope)
	assert.Equal(t, "list", body.Op)
	assert.Contains(t, body.Message, "backend boom")
}

func TestPlansList_StorageErrorHuman(t *testing.T) {
	t.Parallel()
	svc := plans.NewService(failingPlanStorage{err: errors.New("backend boom")}, plans.WithSessionDir(t.TempDir()))

	stdout, stderr, err := executePlans(t, svc, "list")
	requirePlansStatusCode(t, err, 1)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "Error:")
	assert.Contains(t, stderr, "backend boom")
}
