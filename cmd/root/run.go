package root

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"

	"github.com/docker/cagent/pkg/app"
	"github.com/docker/cagent/pkg/cli"
	"github.com/docker/cagent/pkg/config"
	"github.com/docker/cagent/pkg/paths"
	"github.com/docker/cagent/pkg/runtime"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/teamloader"
	"github.com/docker/cagent/pkg/telemetry"
)

type runExecFlags struct {
	agentName      string
	autoApprove    bool
	attachmentPath string
	remoteAddress  string
	connectRPC     bool
	modelOverrides []string
	dryRun         bool
	runConfig      config.RuntimeConfig
	sessionDB      string
	sessionID      string
	recordPath     string
	fakeResponses  string

	// Exec only
	hideToolCalls bool
	outputJSON    bool

	// Run only
	hideToolResults bool
}

func newRunCmd() *cobra.Command {
	var flags runExecFlags

	cmd := &cobra.Command{
		Use:   "run [<agent-file>|<registry-ref>] [message|-]",
		Short: "Run an agent",
		Long:  "Run an agent with the specified configuration and prompt",
		Example: `  cagent run ./agent.yaml
  cagent run ./team.yaml --agent root
  cagent run # built-in default agent
  cagent run ./echo.yaml "INSTRUCTIONS"
  echo "INSTRUCTIONS" | cagent run ./echo.yaml -
  cagent run ./agent.yaml --record  # Records session to auto-generated file`,
		GroupID:           "core",
		ValidArgsFunction: completeRunExec,
		Args:              cobra.RangeArgs(0, 2),
		RunE:              flags.runRunCommand,
	}

	addRunOrExecFlags(cmd, &flags)
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func addRunOrExecFlags(cmd *cobra.Command, flags *runExecFlags) {
	cmd.PersistentFlags().StringVarP(&flags.agentName, "agent", "a", "root", "Name of the agent to run")
	cmd.PersistentFlags().BoolVar(&flags.autoApprove, "yolo", false, "Automatically approve all tool calls without prompting")
	cmd.PersistentFlags().BoolVar(&flags.hideToolResults, "hide-tool-results", false, "Hide tool call results")
	cmd.PersistentFlags().StringVar(&flags.attachmentPath, "attach", "", "Attach an image file to the message")
	cmd.PersistentFlags().StringArrayVar(&flags.modelOverrides, "model", nil, "Override agent model: [agent=]provider/model (repeatable)")
	cmd.PersistentFlags().BoolVar(&flags.dryRun, "dry-run", false, "Initialize the agent without executing anything")
	cmd.PersistentFlags().StringVar(&flags.remoteAddress, "remote", "", "Use remote runtime with specified address")
	cmd.PersistentFlags().BoolVar(&flags.connectRPC, "connect-rpc", false, "Use Connect-RPC protocol for remote communication (requires --remote)")
	cmd.PersistentFlags().StringVarP(&flags.sessionDB, "session-db", "s", filepath.Join(paths.GetHomeDir(), ".cagent", "session.db"), "Path to the session database")
	cmd.PersistentFlags().StringVar(&flags.sessionID, "session", "", "Continue from a previous session by ID")
	cmd.PersistentFlags().StringVar(&flags.fakeResponses, "fake", "", "Replay AI responses from cassette file (for testing)")
	cmd.PersistentFlags().StringVar(&flags.recordPath, "record", "", "Record AI API interactions to cassette file (auto-generates filename if empty)")
	cmd.PersistentFlags().Lookup("record").NoOptDefVal = "true"
	cmd.MarkFlagsMutuallyExclusive("fake", "record")
}

func (f *runExecFlags) runRunCommand(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("run", args)

	ctx := cmd.Context()
	out := cli.NewPrinter(cmd.OutOrStdout())

	tui := isatty.IsTerminal(os.Stdout.Fd())
	return f.runOrExec(ctx, out, args, tui)
}

func (f *runExecFlags) runOrExec(ctx context.Context, out *cli.Printer, args []string, tui bool) error {
	slog.Debug("Starting agent", "agent", f.agentName)

	var agentFileName string
	if len(args) > 0 {
		agentFileName = args[0]
	}

	// Start fake proxy if --fake is specified
	fakeCleanup, err := setupFakeProxy(f.fakeResponses, &f.runConfig)
	if err != nil {
		return err
	}
	defer func() {
		if err := fakeCleanup(); err != nil {
			slog.Error("Failed to cleanup fake proxy", "error", err)
		}
	}()

	// Record AI API interactions to a cassette file if --record flag is specified.
	cassettePath, recordCleanup, err := setupRecordingProxy(f.recordPath, &f.runConfig)
	if err != nil {
		return err
	}
	if cassettePath != "" {
		defer recordCleanup()
		out.Println("Recording mode enabled, cassette: " + cassettePath)
	}

	var (
		rt      runtime.Runtime
		sess    *session.Session
		cleanup func()
	)
	if f.remoteAddress != "" {
		rt, sess, err = f.createRemoteRuntimeAndSession(ctx, agentFileName)
		if err != nil {
			return err
		}
		cleanup = func() {} // Remote runtime doesn't need local cleanup
	} else {
		agentSource, err := config.Resolve(agentFileName)
		if err != nil {
			return err
		}

		loadResult, err := f.loadAgentFrom(ctx, agentSource)
		if err != nil {
			return err
		}

		rt, sess, err = f.createLocalRuntimeAndSession(ctx, loadResult)
		if err != nil {
			return err
		}

		// Setup cleanup for local runtime
		cleanup = func() {
			// Use a fresh context for cleanup since the original may be canceled
			cleanupCtx := context.WithoutCancel(ctx)
			if err := loadResult.Team.StopToolSets(cleanupCtx); err != nil {
				slog.Error("Failed to stop tool sets", "error", err)
			}
		}
	}
	defer cleanup()

	if f.dryRun {
		out.Println("Dry run mode enabled. Agent initialized but will not execute.")
		return nil
	}

	if !tui {
		return f.handleExecMode(ctx, out, rt, sess, args)
	}

	return f.handleRunMode(ctx, rt, sess, args)
}

func (f *runExecFlags) loadAgentFrom(ctx context.Context, agentSource config.Source) (*teamloader.LoadResult, error) {
	result, err := teamloader.LoadWithConfig(ctx, agentSource, &f.runConfig, teamloader.WithModelOverrides(f.modelOverrides))
	if err != nil {
		return nil, err
	}

	return result, nil
}

func (f *runExecFlags) createRemoteRuntimeAndSession(ctx context.Context, originalFilename string) (runtime.Runtime, *session.Session, error) {
	if f.connectRPC {
		return f.createConnectRPCRuntimeAndSession(ctx, originalFilename)
	}
	return f.createHTTPRuntimeAndSession(ctx, originalFilename)
}

func (f *runExecFlags) createConnectRPCRuntimeAndSession(ctx context.Context, originalFilename string) (runtime.Runtime, *session.Session, error) {
	connectClient, err := runtime.NewConnectRPCClient(f.remoteAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create connect-rpc client: %w", err)
	}

	sessTemplate := session.New(
		session.WithToolsApproved(f.autoApprove),
	)

	sess, err := connectClient.CreateSession(ctx, sessTemplate)
	if err != nil {
		return nil, nil, err
	}

	remoteRt, err := runtime.NewRemoteRuntime(connectClient,
		runtime.WithRemoteCurrentAgent(f.agentName),
		runtime.WithRemoteAgentFilename(originalFilename),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create connect-rpc remote runtime: %w", err)
	}

	slog.Debug("Using connect-rpc remote runtime", "address", f.remoteAddress, "agent", f.agentName)
	return remoteRt, sess, nil
}

func (f *runExecFlags) createHTTPRuntimeAndSession(ctx context.Context, originalFilename string) (runtime.Runtime, *session.Session, error) {
	remoteClient, err := runtime.NewClient(f.remoteAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create remote client: %w", err)
	}

	sessTemplate := session.New(
		session.WithToolsApproved(f.autoApprove),
	)

	sess, err := remoteClient.CreateSession(ctx, sessTemplate)
	if err != nil {
		return nil, nil, err
	}

	remoteRt, err := runtime.NewRemoteRuntime(remoteClient,
		runtime.WithRemoteCurrentAgent(f.agentName),
		runtime.WithRemoteAgentFilename(originalFilename),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create remote runtime: %w", err)
	}

	slog.Debug("Using remote runtime", "address", f.remoteAddress, "agent", f.agentName)
	return remoteRt, sess, nil
}

func (f *runExecFlags) createLocalRuntimeAndSession(ctx context.Context, loadResult *teamloader.LoadResult) (runtime.Runtime, *session.Session, error) {
	t := loadResult.Team

	agent, err := t.Agent(f.agentName)
	if err != nil {
		return nil, nil, err
	}

	sessStore, err := session.NewSQLiteSessionStore(f.sessionDB)
	if err != nil {
		return nil, nil, fmt.Errorf("creating session store: %w", err)
	}

	// Create model switcher config for runtime model switching support
	modelSwitcherCfg := &runtime.ModelSwitcherConfig{
		Models:             loadResult.Models,
		Providers:          loadResult.Providers,
		ModelsGateway:      f.runConfig.ModelsGateway,
		EnvProvider:        f.runConfig.EnvProvider(),
		AgentDefaultModels: loadResult.AgentDefaultModels,
	}

	localRt, err := runtime.New(t,
		runtime.WithSessionStore(sessStore),
		runtime.WithCurrentAgent(f.agentName),
		runtime.WithTracer(otel.Tracer(AppName)),
		runtime.WithModelSwitcherConfig(modelSwitcherCfg),
		runtime.WithKRuntime(f.runConfig.KRuntime),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("creating runtime: %w", err)
	}

	var sess *session.Session
	if f.sessionID != "" {
		// Load existing session
		sess, err = sessStore.GetSession(ctx, f.sessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("loading session %q: %w", f.sessionID, err)
		}
		sess.ToolsApproved = f.autoApprove
		sess.HideToolResults = f.hideToolResults

		// Apply any stored model overrides from the session
		if len(sess.AgentModelOverrides) > 0 {
			for agentName, modelRef := range sess.AgentModelOverrides {
				if err := localRt.SetAgentModel(ctx, agentName, modelRef); err != nil {
					slog.Warn("Failed to apply stored model override", "agent", agentName, "model", modelRef, "error", err)
				}
			}
		}

		slog.Debug("Loaded existing session", "session_id", f.sessionID, "agent", f.agentName)
	} else {
		sess = session.New(
			session.WithMaxIterations(agent.MaxIterations()),
			session.WithToolsApproved(f.autoApprove),
			session.WithHideToolResults(f.hideToolResults),
		)
		// Session is stored lazily on first UpdateSession call (when content is added)
		// This avoids creating empty sessions in the database
		slog.Debug("Using local runtime", "agent", f.agentName)
	}

	return localRt, sess, nil
}

func (f *runExecFlags) handleExecMode(ctx context.Context, out *cli.Printer, rt runtime.Runtime, sess *session.Session, args []string) error {
	execArgs := []string{"exec"}
	if len(args) == 2 {
		execArgs = append(execArgs, args[1])
	} else {
		execArgs = append(execArgs, "Please proceed.")
	}

	err := cli.Run(ctx, out, cli.Config{
		AppName:        AppName,
		AttachmentPath: f.attachmentPath,
		HideToolCalls:  f.hideToolCalls,
		OutputJSON:     f.outputJSON,
		AutoApprove:    f.autoApprove,
	}, rt, sess, execArgs)
	if cliErr, ok := err.(cli.RuntimeError); ok {
		return RuntimeError{Err: cliErr.Err}
	}
	return err
}

func readInitialMessage(args []string) (*string, error) {
	if len(args) < 2 {
		return nil, nil
	}

	if args[1] == "-" {
		buf, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("failed to read from stdin: %w", err)
		}
		text := string(buf)
		return &text, nil
	}

	return &args[1], nil
}

func (f *runExecFlags) handleRunMode(ctx context.Context, rt runtime.Runtime, sess *session.Session, args []string) error {
	firstMessage, err := readInitialMessage(args)
	if err != nil {
		return err
	}

	var opts []app.Opt
	if firstMessage != nil {
		opts = append(opts, app.WithFirstMessage(*firstMessage))
	}
	if f.attachmentPath != "" {
		opts = append(opts, app.WithFirstMessageAttachment(f.attachmentPath))
	}

	return runTUI(ctx, rt, sess, opts...)
}
