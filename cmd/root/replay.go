package root

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/replay"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

type replayFlags struct {
	sessionDB  string
	asJSON     bool
	failOnDiff bool
}

func newReplayCmd() *cobra.Command {
	var flags replayFlags

	cmd := &cobra.Command{
		Use:     "replay <session-a> <session-b>",
		Short:   "Compare the behaviour of two recorded sessions",
		GroupID: "advanced",
		Args:    cobra.ExactArgs(2),
		Long: `Compare two recorded sessions and report the first point where the agent
behaved differently.

Comparison is over the sequence of tool calls, not over the assistant's prose.
Model output is nondeterministic: two runs of the same task almost always word
things differently while doing exactly the same work, so diffing text would report
a difference on every comparison. The tool calls are what changed the world, so
they are what is compared.

Reporting stops at the first divergence: everything after it is downstream of that
difference and comparing it produces noise rather than information.`,
		Example: `  docker agent replay <session-a> <session-b>
  docker agent replay <a> <b> --json | jq '.divergence.turn_index'
  docker agent replay <a> <b> --fail-on-divergence`,
		RunE: flags.run,
	}

	cmd.Flags().StringVarP(&flags.sessionDB, "session-db", "s", "", "Path to the session database (default: <data-dir>/session.db)")
	cmd.Flags().BoolVar(&flags.asJSON, "json", false, "Emit the comparison as JSON")
	cmd.Flags().BoolVar(&flags.failOnDiff, "fail-on-divergence", false, "Exit non-zero when the two sessions diverge")

	return cmd
}

func (f *replayFlags) run(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	dbPath, err := pathx.ExpandHomeDir(sessionDBPath(f.sessionDB))
	if err != nil {
		return err
	}

	store, err := sqlitestore.New(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("opening session store: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			slog.ErrorContext(ctx, "Failed to close session store", "error", err)
		}
	}()

	sessA, err := store.GetSession(ctx, args[0])
	if err != nil {
		return fmt.Errorf("reading session %q: %w", args[0], err)
	}
	sessB, err := store.GetSession(ctx, args[1])
	if err != nil {
		return fmt.Errorf("reading session %q: %w", args[1], err)
	}

	result := replay.CompareSessions(sessA, sessB)
	if err := renderReplay(cmd.OutOrStdout(), result, args[0], args[1], f.asJSON); err != nil {
		return err
	}

	if f.failOnDiff && !result.Identical() {
		return errors.New("sessions diverged")
	}
	return nil
}

// renderReplay writes the comparison as JSON or as text.
func renderReplay(w io.Writer, result replay.Result, nameA, nameB string, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	replay.PrintResult(w, result, nameA, nameB)
	return nil
}
