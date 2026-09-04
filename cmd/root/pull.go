package root

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/protect"
	"github.com/docker/docker-agent/pkg/remote"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type pullFlags struct {
	force   bool
	keyFile string
}

func newPullCmd() *cobra.Command {
	var flags pullFlags

	cmd := &cobra.Command{
		Use:   "pull <registry-ref>",
		Short: "Pull an agent from an OCI registry",
		Long: `Pull an agent configuration file from an OCI registry.

With --key, the pulled agent YAML is verified against the protection recorded
by 'share push --key': a signature is checked with the same secret or the
matching public key; an encrypted copy (push --encrypt) is decrypted with the
same secret or the matching private key and compared to the YAML. With an
asymmetric key the artifact must carry a signature. The pull fails if the
artifact is unprotected or the check does not pass.`,
		Args: cobra.ExactArgs(1),
		RunE: flags.runPullCommand,
	}

	cmd.PersistentFlags().BoolVar(&flags.force, "force", false, "Force pull even if the configuration already exists locally")
	cmd.Flags().StringVar(&flags.keyFile, "key", "", "Path to a key file (PEM/OpenSSH) or symmetric secret used to verify the agent (or set "+envEncryptKey+" with the secret inline)")

	return cmd
}

func (f *pullFlags) runPullCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "share", append([]string{"pull"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "share", append([]string{"pull"}, args...), commandErr)
	}()

	out := cli.NewPrinter(cmd.OutOrStdout())
	registryRef := args[0]
	slog.DebugContext(ctx, "Starting pull", "registry_ref", registryRef)

	var key *protect.Key
	if f.keyFile != "" {
		var err error
		if key, err = protect.LoadKey(f.keyFile); err != nil {
			return err
		}
	} else if secret := os.Getenv(envEncryptKey); secret != "" {
		var err error
		if key, err = protect.ParseKey([]byte(secret)); err != nil {
			return err
		}
	}

	out.Println("Pulling agent", registryRef)

	_, err := remote.Pull(ctx, registryRef, f.force)
	if err != nil {
		return fmt.Errorf("failed to pull artifact: %w", err)
	}

	store, err := content.NewStore()
	if err != nil {
		return fmt.Errorf("failed to open content store: %w", err)
	}
	yamlFile, err := store.GetArtifact(registryRef)
	if err != nil {
		return fmt.Errorf("failed to get agent yaml: %w", err)
	}

	if key != nil {
		metadata, err := store.GetArtifactMetadata(registryRef)
		if err != nil {
			return fmt.Errorf("failed to get artifact metadata: %w", err)
		}
		verified, err := key.VerifyAnnotations(metadata.Annotations, []byte(yamlFile))
		if err != nil {
			return fmt.Errorf("verifying %s: %w", registryRef, err)
		}
		out.Printf("Verified %s\n", verified)
	}

	agentName := strings.ReplaceAll(registryRef, "/", "_")
	fileName := agentName + ".yaml"

	if err := os.WriteFile(fileName, []byte(yamlFile), 0o644); err != nil { //nolint:gosec // pulled agent yaml is meant to be readable
		return err
	}

	out.Printf("Agent saved to %s\n", fileName)

	return nil
}
