package root

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/content"
	"github.com/docker/docker-agent/pkg/oci"
	"github.com/docker/docker-agent/pkg/protect"
	"github.com/docker/docker-agent/pkg/remote"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type pushFlags struct {
	keyFile string
	encrypt bool
}

func newPushCmd() *cobra.Command {
	var flags pushFlags

	cmd := &cobra.Command{
		Use:   "push <agent-file> <registry-ref>",
		Short: "Push an agent to an OCI registry",
		Long: `Push an agent configuration file to an OCI registry.

With --key, the agent YAML is protected and the proof is stored as manifest
annotations; the YAML itself is always pushed in clear. The key kind is
detected from the file: PEM or OpenSSH keys (Ed25519, ECDSA, RSA) are
asymmetric, anything else is a raw symmetric secret of at least 16 bytes
(e.g. 'openssl rand -hex 32 > agent.key') that must not contain PEM or
OpenSSH key markers.

  Default (sign):  a signature (private key) or MAC (secret) is recorded.
                   Holders of the public key or secret can check integrity
                   with 'share pull --key'.
  --encrypt:       an encrypted copy of the whole YAML is recorded as well,
                   so holders of the secret or private key can also recover
                   the YAML from the annotation alone. With an asymmetric
                   key this needs the private key and still records a
                   signature, since a copy encrypted to a public key proves
                   nothing about who published it. Ed25519 cannot encrypt.`,
		Args: cobra.ExactArgs(2),
		RunE: flags.runPushCommand,
	}

	cmd.Flags().StringVar(&flags.keyFile, "key", "", "Path to a key file (PEM/OpenSSH) or symmetric secret used to protect the agent (or set "+envEncryptKey+" with the secret inline)")
	cmd.Flags().BoolVar(&flags.encrypt, "encrypt", false, "Also embed an encrypted copy of the agent in the annotations (requires --key)")

	return cmd
}

// envEncryptKey is an alternative to --key for callers that prefer not to write
// the secret to a file: its value is used as the symmetric secret directly
// (never a file path). The --key flag takes precedence when both are set.
const envEncryptKey = "DOCKER_AGENT_ENCRYPT_KEY"

// resolveKey loads the protection key from the --key flag (a file path) or,
// when the flag is empty, from the [envEncryptKey] environment variable (an
// inline symmetric secret). It returns a nil key and nil error when neither is
// set, so callers can treat "no key" as "no protection".
func (f *pushFlags) resolveKey() (*protect.Key, error) {
	if f.keyFile != "" {
		return protect.LoadKey(f.keyFile)
	}
	if secret := os.Getenv(envEncryptKey); secret != "" {
		return protect.ParseKey([]byte(secret))
	}
	return nil, nil
}

func (f *pushFlags) protection() (oci.PackageOption, error) {
	key, err := f.resolveKey()
	if err != nil {
		return nil, err
	}
	if key == nil {
		if f.encrypt {
			return nil, fmt.Errorf("--encrypt requires --key or %s", envEncryptKey)
		}
		return nil, nil
	}
	mode := protect.ModeSign
	if f.encrypt {
		mode = protect.ModeEncrypt
	}
	if err := key.Supports(mode); err != nil {
		return nil, fmt.Errorf("%s: %w", f.keyFile, err)
	}
	return oci.WithProtection(key, mode), nil
}

func (f *pushFlags) runPushCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "share", append([]string{"push"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "share", append([]string{"push"}, args...), commandErr)
	}()

	agentFilename := args[0]
	tag := args[1]
	out := cli.NewPrinter(cmd.OutOrStdout())

	var packageOpts []oci.PackageOption
	protection, err := f.protection()
	if err != nil {
		return err
	}
	if protection != nil {
		packageOpts = append(packageOpts, protection)
	}

	store, err := content.NewStore()
	if err != nil {
		return err
	}

	agentSource, err := sources.Resolve(agentFilename, nil)
	if err != nil {
		return fmt.Errorf("resolving agent file: %w", err)
	}

	_, err = oci.PackageFileAsOCIToStore(ctx, agentSource, tag, store, packageOpts...)
	if err != nil {
		return fmt.Errorf("failed to build artifact: %w", err)
	}

	slog.DebugContext(ctx, "Starting push", "registry_ref", tag, "protected", protection != nil, "encrypt", f.encrypt)

	out.Printf("Pushing agent %s to %s\n", agentFilename, tag)

	err = remote.Push(ctx, tag)
	if err != nil {
		return fmt.Errorf("failed to push artifact: %w", err)
	}

	out.Printf("Successfully pushed artifact to %s\n", tag)
	return nil
}
