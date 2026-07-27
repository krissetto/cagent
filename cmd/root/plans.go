package root

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/docker/cli/cli"
	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
)

// plansSchemaVersion stamps every JSON document the plans commands emit
// (stdout results and stderr errors) so consumers can detect the format.
const plansSchemaVersion = "1"

// plansConflictExitCode is the exit code for version conflicts, distinct from
// the generic failure code 1 so scripts can branch on "re-read and retry".
const plansConflictExitCode = 3

// Stable machine-readable error codes of the --json stderr contract.
const (
	plansErrCodeConflict    = "conflict"
	plansErrCodeNotFound    = "not_found"
	plansErrCodeInvalid     = "invalid_argument"
	plansErrCodeUnsupported = "unsupported"
	plansErrCodeCorrupt     = "corrupt"
	plansErrCodeStorage     = "storage"
	plansErrCodeUnknown     = "error"
)

// plansCmdOption customizes newPlansCmd; used by tests to inject the service.
type plansCmdOption func(*plansOptions)

type plansOptions struct {
	service plans.Service
}

// withPlansService injects the plans.Service every subcommand uses, so tests
// run hermetically against temp-backed storage instead of the user's data.
func withPlansService(svc plans.Service) plansCmdOption {
	return func(o *plansOptions) { o.service = svc }
}

// resolveService builds the production service. It must only run at RunE
// time: plan.SharedStorage() resolves the plans directory on first use, so
// touching it before the root persistent pre-run has applied --data-dir would
// capture (and memoize process-wide) the wrong default directory.
func (o *plansOptions) resolveService() plans.Service {
	if o.service != nil {
		return o.service
	}
	return plans.NewService(plan.SharedStorage())
}

func newPlansCmd(opts ...plansCmdOption) *cobra.Command {
	options := &plansOptions{}
	for _, opt := range opts {
		opt(options)
	}

	cmd := &cobra.Command{
		Use:   "plans",
		Short: "Manage shared and session plans",
		Long: `Manage the plans agents collaborate on, from the host.

Two kinds of plans exist:

  - shared plans: the named, versioned documents of the plan toolset,
    collaborated on across sessions. Fully manageable here.
  - session plans: the single per-session plan of the "draft, review,
    execute" workflow. Read-only here (list, get, export); they belong to
    their session and are changed from within it.

Mutations guard against concurrent edits: pass --expected-version <n> (the
version from a previous get or list) to fail with exit code 3 when the plan
changed in the meantime, or pass --force to deliberately write without the
guard. The CLI never prompts.

Every subcommand accepts --json for stable machine-readable output on stdout;
failures are then reported as a single JSON object on stderr.`,
		Example: `  docker-agent plans list
  docker-agent plans create release --file ./plan.md --title "Release plan"
  docker-agent plans get release
  docker-agent plans update release --file ./plan.md --expected-version 1
  docker-agent plans status release done --expected-version 2
  docker-agent plans export release --output ./plan.md
  docker-agent plans delete release --expected-version 3
  docker-agent plans get --session <session-id>`,
		GroupID:      "advanced",
		SilenceUsage: true,
	}

	cmd.AddCommand(
		newPlansListCmd(options),
		newPlansGetCmd(options),
		newPlansCreateCmd(options),
		newPlansUpdateCmd(options),
		newPlansStatusCmd(options),
		newPlansExportCmd(options),
		newPlansDeleteCmd(options),
	)

	return cmd
}

// runPlans adapts a subcommand handler into a cobra RunE: it tracks
// telemetry, renders failures (human text, or the JSON error contract when
// --json is set), and maps them to process exit codes via cli.StatusError.
func (o *plansOptions) runPlans(sub string, jsonOut *bool, handler func(cmd *cobra.Command, svc plans.Service, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		trackArgs := append([]string{sub}, args...)
		telemetry.TrackCommand(ctx, "plans", trackArgs)
		err := handler(cmd, o.resolveService(), args)
		telemetry.TrackCommandError(ctx, "plans", trackArgs, err)
		if err == nil {
			return nil
		}
		// The failure is fully rendered here; silence cobra so stderr stays a
		// single message (a single JSON object in --json mode). Only the exit
		// code travels up: main honours cli.StatusError.
		cmd.SilenceErrors = true
		printPlansError(cmd.ErrOrStderr(), err, *jsonOut)
		return cli.StatusError{StatusCode: plansExitCode(err), Cause: err}
	}
}

func plansExitCode(err error) int {
	var conflict *plans.ConflictError
	if errors.As(err, &conflict) {
		return plansConflictExitCode
	}
	return 1
}

// plansUsageError marks CLI-level input mistakes (bad flag combinations,
// unreadable --file paths) so they map to the invalid_argument error code.
type plansUsageError struct{ msg string }

func (e *plansUsageError) Error() string { return e.msg }

func plansUsagef(format string, a ...any) error {
	return &plansUsageError{msg: fmt.Sprintf(format, a...)}
}

// plansErrorBody is the "error" object of the JSON stderr contract. Scope,
// name, op, and the conflict versions are included where the failure carries
// them.
type plansErrorBody struct {
	Code            string      `json:"code"`
	Message         string      `json:"message"`
	Scope           plans.Scope `json:"scope,omitempty"`
	Name            string      `json:"name,omitempty"`
	Op              string      `json:"op,omitempty"`
	ExpectedVersion *int        `json:"expected_version,omitempty"`
	CurrentVersion  *int        `json:"current_version,omitempty"`
}

type plansErrorDocument struct {
	SchemaVersion string         `json:"schema_version"`
	Error         plansErrorBody `json:"error"`
}

// plansErrorBodyFor classifies by the typed errors of pkg/plans, never by
// error text.
func plansErrorBodyFor(err error) plansErrorBody {
	var (
		conflict    *plans.ConflictError
		notFound    *plans.NotFoundError
		unsupported *plans.UnsupportedError
		corrupt     *plans.CorruptError
		storageErr  *plans.StorageError
		validation  *plans.ValidationError
		usage       *plansUsageError
	)
	switch {
	case errors.As(err, &conflict):
		// Conflicts only exist for shared plans, which are the only versioned
		// scope.
		return plansErrorBody{
			Code:            plansErrCodeConflict,
			Message:         err.Error(),
			Scope:           plans.ScopeShared,
			Name:            conflict.Name,
			ExpectedVersion: new(conflict.Expected),
			CurrentVersion:  new(conflict.Current),
		}
	case errors.As(err, &notFound):
		return plansErrorBody{Code: plansErrCodeNotFound, Message: err.Error(), Scope: notFound.Scope, Name: notFound.Name}
	case errors.As(err, &unsupported):
		return plansErrorBody{Code: plansErrCodeUnsupported, Message: err.Error(), Scope: unsupported.Scope, Op: unsupported.Op}
	case errors.As(err, &corrupt):
		return plansErrorBody{Code: plansErrCodeCorrupt, Message: err.Error(), Scope: corrupt.Scope, Name: corrupt.Name}
	case errors.As(err, &storageErr):
		return plansErrorBody{Code: plansErrCodeStorage, Message: err.Error(), Scope: storageErr.Scope, Op: storageErr.Op}
	case errors.As(err, &validation), errors.As(err, &usage):
		return plansErrorBody{Code: plansErrCodeInvalid, Message: err.Error()}
	default:
		return plansErrorBody{Code: plansErrCodeUnknown, Message: err.Error()}
	}
}

func printPlansError(w io.Writer, err error, jsonOut bool) {
	if jsonOut {
		// Single-line JSON so stderr is one machine-detectable object.
		_ = json.NewEncoder(w).Encode(plansErrorDocument{SchemaVersion: plansSchemaVersion, Error: plansErrorBodyFor(err)})
		return
	}
	fmt.Fprintln(w, "Error:", err.Error())
}

// planRefFlags selects the plan a subcommand addresses: the shared plan named
// by the positional argument (the default), or a session's plan via
// --session. --scope disambiguates explicitly; --session alone implies
// session scope.
type planRefFlags struct {
	scope   string
	session string
}

func (f *planRefFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.scope, "scope", "", `Plan scope: "shared" or "session" (default "shared"; "session" is implied by --session)`)
	cmd.Flags().StringVar(&f.session, "session", "", "Session ID whose plan to address (session scope)")
}

func (f *planRefFlags) sessionSelected() bool {
	return f.session != "" || f.scope == string(plans.ScopeSession)
}

func (f *planRefFlags) resolve(name string) (plans.Ref, error) {
	scope := plans.Scope(f.scope)
	if f.scope == "" {
		scope = plans.ScopeShared
		if f.session != "" {
			scope = plans.ScopeSession
		}
	}
	switch scope {
	case plans.ScopeShared:
		if f.session != "" {
			return plans.Ref{}, plansUsagef("--session selects a session plan: drop --scope shared or use --scope session")
		}
		if name == "" {
			return plans.Ref{}, plansUsagef("a plan name is required for shared plans")
		}
		return plans.SharedRef(name), nil
	case plans.ScopeSession:
		if f.session == "" {
			return plans.Ref{}, plansUsagef("--scope session requires --session <id>")
		}
		if name != "" {
			return plans.Ref{}, plansUsagef("session plans are addressed by --session <id>, not by name; drop %q", name)
		}
		return plans.SessionRef(f.session), nil
	default:
		return plans.Ref{}, plansUsagef("invalid --scope %q: use %q or %q", f.scope, plans.ScopeShared, plans.ScopeSession)
	}
}

// planRefName is the plan's identity within its scope, for messages and the
// delete document.
func planRefName(ref plans.Ref) string {
	if ref.Scope == plans.ScopeSession {
		return ref.SessionID
	}
	return ref.Name
}

// planGuardFlags implements the mutation write-guard: --expected-version
// enables optimistic locking, --force deliberately opts out of it. Exactly
// one must be given (enforced by cobra before RunE), so a nil expected
// version is never sent by accident.
type planGuardFlags struct {
	expectedVersion int
	force           bool
}

func (f *planGuardFlags) register(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.expectedVersion, "expected-version", 0, "Version the plan is expected to be at; the write fails with a version conflict (exit code 3) when it changed")
	cmd.Flags().BoolVar(&f.force, "force", false, "Write unconditionally, without the optimistic-lock guard")
	cmd.MarkFlagsOneRequired("expected-version", "force")
	cmd.MarkFlagsMutuallyExclusive("expected-version", "force")
}

// expected returns the guard to send to the service: the validated expected
// version, or nil for a deliberate --force.
func (f *planGuardFlags) expected() (*int, error) {
	if f.force {
		return nil, nil
	}
	if f.expectedVersion < 1 {
		return nil, plansUsagef("--expected-version must be at least 1, got %d", f.expectedVersion)
	}
	return &f.expectedVersion, nil
}

// readPlanContent loads the new plan body from path, or from stdin when path
// is "-" so scripts can pipe content in. The CLI never prompts: content only
// ever arrives through --file.
func readPlanContent(cmd *cobra.Command, path string) (string, error) {
	if path == "-" {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", plansUsagef("reading plan content from stdin: %v", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", plansUsagef("reading plan content from %q: %v", path, err)
	}
	return string(data), nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func formatPlanVersion(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

func formatPlanTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writePlansJSON(w io.Writer, doc any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

type plansListDocument struct {
	SchemaVersion string       `json:"schema_version"`
	Plans         []plans.Plan `json:"plans"`
	Warnings      []string     `json:"warnings,omitempty"`
}

type plansPlanDocument struct {
	SchemaVersion string     `json:"schema_version"`
	Plan          plans.Plan `json:"plan"`
}

type plansExportDocument struct {
	SchemaVersion string             `json:"schema_version"`
	Export        plans.ExportResult `json:"export"`
}

type plansDeletedDocument struct {
	SchemaVersion string       `json:"schema_version"`
	Deleted       plansRefBody `json:"deleted"`
}

type plansRefBody struct {
	Scope plans.Scope `json:"scope"`
	Name  string      `json:"name"`
}

func printPlanMutation(cmd *cobra.Command, jsonOut bool, p plans.Plan, verb string) error {
	if jsonOut {
		return writePlansJSON(cmd.OutOrStdout(), plansPlanDocument{SchemaVersion: plansSchemaVersion, Plan: p})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s plan %q (version %s)\n", verb, p.Scope, p.Name, formatPlanVersion(p.Version))
	return nil
}

// printPlanMetadata renders the concise one-line metadata of a plan. `get`
// sends it to stderr so stdout stays pure content while the metadata remains
// visible.
func printPlanMetadata(w io.Writer, p plans.Plan) {
	details := make([]string, 0, 5)
	if p.Title != "" {
		details = append(details, "title: "+p.Title)
	}
	if p.Status != "" {
		details = append(details, "status: "+p.Status)
	}
	details = append(details, "version: "+formatPlanVersion(p.Version))
	if !p.UpdatedAt.IsZero() {
		details = append(details, "updated: "+formatPlanTime(p.UpdatedAt))
	}
	if p.Path != "" {
		details = append(details, "path: "+p.Path)
	}
	fmt.Fprintf(w, "%s plan %q (%s)\n", p.Scope, p.Name, strings.Join(details, ", "))
}

func printPlansTable(w io.Writer, list []plans.Plan) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No plans found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 3, ' ', 0)
	fmt.Fprintln(tw, "SCOPE\tNAME\tSTATUS\tVERSION\tUPDATED\tTITLE")
	for _, p := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Scope, p.Name, orDash(p.Status), formatPlanVersion(p.Version), formatPlanTime(p.UpdatedAt), orDash(p.Title))
	}
	tw.Flush()
}

func newPlansListCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json    bool
		session string
	}

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List plans",
		Long: `List every shared plan with its metadata (content is not included).

With --session <id>, that session's plan is listed first when it exists; a
session without a plan is simply not listed. Plans that exist but cannot be
read are reported as warnings on stderr (in the "warnings" field with --json)
so they are never mistaken for missing.`,
		Args: cobra.NoArgs,
	}
	cmd.RunE = o.runPlans("list", &flags.json, func(cmd *cobra.Command, svc plans.Service, _ []string) error {
		result, err := svc.List(cmd.Context(), plans.ListOptions{SessionID: flags.session})
		if err != nil {
			return err
		}
		// Defensive against injected services: an empty listing must encode
		// as [] rather than null.
		if result.Plans == nil {
			result.Plans = []plans.Plan{}
		}
		if flags.json {
			return writePlansJSON(cmd.OutOrStdout(), plansListDocument{
				SchemaVersion: plansSchemaVersion,
				Plans:         result.Plans,
				Warnings:      result.Warnings,
			})
		}
		printPlansTable(cmd.OutOrStdout(), result.Plans)
		for _, warning := range result.Warnings {
			fmt.Fprintln(cmd.ErrOrStderr(), "Warning:", warning)
		}
		return nil
	})

	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")
	cmd.Flags().StringVar(&flags.session, "session", "", "Also include this session's plan when it exists")

	return cmd
}

func newPlansGetCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json bool
		ref  planRefFlags
	}

	cmd := &cobra.Command{
		Use:   "get [<name>]",
		Short: "Print a plan's content and metadata",
		Long: `Print a plan: its content goes to stdout and a concise metadata line goes
to stderr, so redirecting stdout captures the content alone (use export for a
byte-exact file copy).

By default <name> addresses a shared plan. Use --session <id> to print that
session's plan instead; the name is then omitted.`,
		Example: `  docker-agent plans get release
  docker-agent plans get release --json
  docker-agent plans get --session <session-id>
  docker-agent plans get --scope session --session <session-id>`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = o.runPlans("get", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		ref, err := flags.ref.resolve(firstArg(args))
		if err != nil {
			return err
		}
		p, err := svc.Get(cmd.Context(), ref)
		if err != nil {
			return err
		}
		if flags.json {
			return writePlansJSON(cmd.OutOrStdout(), plansPlanDocument{SchemaVersion: plansSchemaVersion, Plan: p})
		}
		printPlanMetadata(cmd.ErrOrStderr(), p)
		out := cmd.OutOrStdout()
		fmt.Fprint(out, p.Content)
		if !strings.HasSuffix(p.Content, "\n") {
			fmt.Fprintln(out)
		}
		return nil
	})

	flags.ref.register(cmd)
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")

	return cmd
}

func newPlansCreateCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json   bool
		file   string
		title  string
		author string
		status string
	}

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new shared plan",
		Long: `Create a new shared plan with content from --file (required; use "-" to
read stdin). The CLI never prompts for content.

Create is create-only: when a plan with the same name already exists the
command fails with a version conflict (exit code 3) instead of overwriting.`,
		Example: `  docker-agent plans create release --file ./plan.md --title "Release plan"
  cat plan.md | docker-agent plans create release --file -`,
		Args: cobra.ExactArgs(1),
	}
	cmd.RunE = o.runPlans("create", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		content, err := readPlanContent(cmd, flags.file)
		if err != nil {
			return err
		}
		p, err := svc.Create(cmd.Context(), plans.CreateRequest{
			Ref:     plans.SharedRef(args[0]),
			Content: content,
			Title:   flags.title,
			Author:  flags.author,
			Status:  flags.status,
		})
		if err != nil {
			return err
		}
		return printPlanMutation(cmd, flags.json, p, "Created")
	})

	cmd.Flags().StringVar(&flags.file, "file", "", `File with the plan content ("-" reads stdin); required`)
	cmd.Flags().StringVar(&flags.title, "title", "", "Human-readable plan title")
	cmd.Flags().StringVar(&flags.author, "author", "", "Label identifying who wrote the plan")
	cmd.Flags().StringVar(&flags.status, "status", "", "Free-form lifecycle status (e.g. draft, in-progress)")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("file")

	return cmd
}

func newPlansUpdateCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json   bool
		file   string
		title  string
		author string
		status string
		ref    planRefFlags
		guard  planGuardFlags
	}

	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Replace the content of an existing shared plan",
		Long: `Replace the content of an existing shared plan with the contents of --file
(required; use "-" to read stdin). Update never creates a plan.

Metadata flags that are omitted keep their previous value; passing them
(including an empty value) overwrites it.

Exactly one of --expected-version or --force must be given: the former fails
with exit code 3 when the plan changed since it was read, the latter
deliberately replaces it unconditionally. Session plans cannot be updated.`,
		Example: `  docker-agent plans update release --file ./plan.md --expected-version 1
  docker-agent plans update release --file ./plan.md --force --status in-progress`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = o.runPlans("update", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		ref, err := flags.ref.resolve(firstArg(args))
		if err != nil {
			return err
		}
		expected, err := flags.guard.expected()
		if err != nil {
			return err
		}
		content, err := readPlanContent(cmd, flags.file)
		if err != nil {
			return err
		}
		req := plans.UpdateRequest{Ref: ref, Content: content, ExpectedVersion: expected}
		// Only changed metadata flags overwrite; omitted ones are preserved.
		if cmd.Flags().Changed("title") {
			req.Title = &flags.title
		}
		if cmd.Flags().Changed("author") {
			req.Author = &flags.author
		}
		if cmd.Flags().Changed("status") {
			req.Status = &flags.status
		}
		p, err := svc.Update(cmd.Context(), req)
		if err != nil {
			return err
		}
		return printPlanMutation(cmd, flags.json, p, "Updated")
	})

	flags.ref.register(cmd)
	flags.guard.register(cmd)
	cmd.Flags().StringVar(&flags.file, "file", "", `File with the new plan content ("-" reads stdin); required`)
	cmd.Flags().StringVar(&flags.title, "title", "", "New plan title (omit to preserve the current one)")
	cmd.Flags().StringVar(&flags.author, "author", "", "New author label (omit to preserve the current one)")
	cmd.Flags().StringVar(&flags.status, "status", "", "New lifecycle status (omit to preserve the current one)")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("file")

	return cmd
}

func newPlansStatusCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json  bool
		ref   planRefFlags
		guard planGuardFlags
	}

	cmd := &cobra.Command{
		Use:   "status <name> <status>",
		Short: "Set the status of an existing shared plan",
		Long: `Set a shared plan's free-form status (e.g. "in-progress", "blocked",
"done") without touching its body. Setting the status is a write and bumps
the version.

Exactly one of --expected-version or --force must be given. Session plans
have no status.`,
		Example: `  docker-agent plans status release done --expected-version 2
  docker-agent plans status release blocked --force`,
		Args: cobra.RangeArgs(1, 2),
	}
	cmd.RunE = o.runPlans("status", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		var name, status string
		if flags.ref.sessionSelected() {
			// Session plans have no name, so the only positional is the
			// status; the service then rejects the mutation as unsupported.
			if len(args) != 1 {
				return plansUsagef("session plans take only a status: plans status --session <id> <status>")
			}
			status = args[0]
		} else {
			if len(args) != 2 {
				return plansUsagef("shared plans take a name and a status: plans status <name> <status>")
			}
			name, status = args[0], args[1]
		}
		ref, err := flags.ref.resolve(name)
		if err != nil {
			return err
		}
		expected, err := flags.guard.expected()
		if err != nil {
			return err
		}
		p, err := svc.SetStatus(cmd.Context(), plans.SetStatusRequest{Ref: ref, Status: status, ExpectedVersion: expected})
		if err != nil {
			return err
		}
		if flags.json {
			return writePlansJSON(cmd.OutOrStdout(), plansPlanDocument{SchemaVersion: plansSchemaVersion, Plan: p})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Set status of %s plan %q to %q (version %s)\n",
			p.Scope, p.Name, p.Status, formatPlanVersion(p.Version))
		return nil
	})

	flags.ref.register(cmd)
	flags.guard.register(cmd)
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")

	return cmd
}

func newPlansExportCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json   bool
		output string
		ref    planRefFlags
	}

	cmd := &cobra.Command{
		Use:   "export [<name>]",
		Short: "Write a plan's content to a file",
		Long: `Write a plan's content, byte-exact, to --output (required). Parent
directories are created and the write is atomic, so a reader never observes
a partial export. Works for both scopes: shared plans by name, a session's
plan via --session <id>.`,
		Example: `  docker-agent plans export release --output ./plan.md
  docker-agent plans export --session <session-id> --output ./plan.md`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = o.runPlans("export", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		ref, err := flags.ref.resolve(firstArg(args))
		if err != nil {
			return err
		}
		result, err := svc.Export(cmd.Context(), plans.ExportRequest{Ref: ref, Path: flags.output})
		if err != nil {
			return err
		}
		if flags.json {
			return writePlansJSON(cmd.OutOrStdout(), plansExportDocument{SchemaVersion: plansSchemaVersion, Export: result})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Exported %s plan %q (version %s) to %s (%d bytes)\n",
			result.Scope, result.Name, formatPlanVersion(result.Version), result.Path, result.BytesWritten)
		return nil
	})

	flags.ref.register(cmd)
	cmd.Flags().StringVar(&flags.output, "output", "", "Destination file for the plan content; required")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")
	_ = cmd.MarkFlagRequired("output")

	return cmd
}

func newPlansDeleteCmd(o *plansOptions) *cobra.Command {
	var flags struct {
		json  bool
		ref   planRefFlags
		guard planGuardFlags
	}

	cmd := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a shared plan",
		Long: `Delete a shared plan. The CLI never prompts, so exactly one of
--expected-version or --force must be given: the former fails with exit
code 3 when the plan changed since it was read (leaving it in place), the
latter deletes unconditionally — which is also how a corrupt plan is
recovered. Session plans cannot be deleted from the host.`,
		Example: `  docker-agent plans delete release --expected-version 3
  docker-agent plans delete release --force`,
		Args: cobra.MaximumNArgs(1),
	}
	cmd.RunE = o.runPlans("delete", &flags.json, func(cmd *cobra.Command, svc plans.Service, args []string) error {
		ref, err := flags.ref.resolve(firstArg(args))
		if err != nil {
			return err
		}
		expected, err := flags.guard.expected()
		if err != nil {
			return err
		}
		if err := svc.Delete(cmd.Context(), plans.DeleteRequest{Ref: ref, ExpectedVersion: expected}); err != nil {
			return err
		}
		if flags.json {
			return writePlansJSON(cmd.OutOrStdout(), plansDeletedDocument{
				SchemaVersion: plansSchemaVersion,
				Deleted:       plansRefBody{Scope: ref.Scope, Name: planRefName(ref)},
			})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s plan %q\n", ref.Scope, planRefName(ref))
		return nil
	})

	flags.ref.register(cmd)
	flags.guard.register(cmd)
	cmd.Flags().BoolVar(&flags.json, "json", false, "Output as JSON")

	return cmd
}
