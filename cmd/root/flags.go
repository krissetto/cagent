package root

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/cagent/pkg/config"
)

const (
	flagModelsGateway = "models-gateway"
	envModelsGateway  = "CAGENT_MODELS_GATEWAY"
)

func addRuntimeConfigFlags(cmd *cobra.Command, runConfig *config.RuntimeConfig) {
	addGatewayFlags(cmd, runConfig)
	cmd.PersistentFlags().StringSliceVar(&runConfig.EnvFiles, "env-from-file", nil, "Set environment variables from file")
	cmd.PersistentFlags().BoolVar(&runConfig.GlobalCodeMode, "code-mode-tools", false, "Provide a single tool to call other tools via Javascript")
	cmd.PersistentFlags().StringVar(&runConfig.WorkingDir, "working-dir", "", "Set the working directory for the session (applies to tools and relative paths)")
	cmd.PersistentFlags().BoolVar(&runConfig.KRuntime, "kruntime", false, "Enable task-based context runtime (experimental)")
}

func setupWorkingDirectory(workingDir string) error {
	if workingDir == "" {
		return nil
	}

	absWd, err := filepath.Abs(workingDir)
	if err != nil {
		return fmt.Errorf("invalid working directory: %w", err)
	}

	info, err := os.Stat(absWd)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("working directory does not exist or is not a directory: %s", absWd)
	}

	if err := os.Chdir(absWd); err != nil {
		return fmt.Errorf("failed to change working directory: %w", err)
	}

	_ = os.Setenv("PWD", absWd)
	slog.Debug("Working directory set", "path", absWd)

	return nil
}

func canonize(endpoint string) string {
	return strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
}

func logFlagShadowing(envValue, varName, flagName string) {
	if envValue != "" {
		_, _ = fmt.Fprintf(os.Stderr, "CLI flag --%s set, overriding environment variable %s\n", flagName, varName)
	}
}

func addGatewayFlags(cmd *cobra.Command, runConfig *config.RuntimeConfig) {
	cmd.PersistentFlags().StringVar(&runConfig.ModelsGateway, flagModelsGateway, "", "Set the models gateway address")

	persistentPreRunE := cmd.PersistentPreRunE
	cmd.PersistentPreRunE = func(_ *cobra.Command, args []string) error {
		if runConfig.ModelsGateway != "" {
			// CLI flag takes precedence over environment variable
			logFlagShadowing(os.Getenv(envModelsGateway), envModelsGateway, flagModelsGateway)
		} else if gateway := os.Getenv(envModelsGateway); gateway != "" {
			runConfig.ModelsGateway = gateway
		}

		// Ensure the gateway url is canonical.
		runConfig.ModelsGateway = canonize(runConfig.ModelsGateway)

		// Setup working directory
		if err := setupWorkingDirectory(runConfig.WorkingDir); err != nil {
			return err
		}

		// First call the original persistentPreRunE if it exists (from this command)
		if persistentPreRunE != nil {
			return persistentPreRunE(cmd, args)
		}

		// If this command doesn't have its own persistentPreRunE, check if the parent has one
		// This ensures parent PersistentPreRunE is called for child commands
		if cmd.Parent() != nil && cmd.Parent().PersistentPreRunE != nil {
			return cmd.Parent().PersistentPreRunE(cmd, args)
		}

		return nil
	}
}
