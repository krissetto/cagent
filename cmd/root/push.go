package root

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
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
detected from the file: PEM or OpenSSH keys are asymmetric, anything else is
a raw symmetric secret.

  Default (sign):  a signature (private key) or MAC (secret) is recorded.
                   Holders of the public key or secret can check integrity
                   with 'share pull --key'.
  --encrypt:       an encrypted copy of the whole YAML is recorded instead,
                   encrypted with the secret or to the given public key.
                   Holders of the secret or private key can check integrity
                   and recover the YAML from the annotation alone.

Signing needs Ed25519, ECDSA or RSA. Encrypting needs X25519, ECDSA or RSA.`,
		Args: cobra.ExactArgs(2),
		RunE: flags.runPushCommand,
	}

	cmd.Flags().StringVar(&flags.keyFile, "key", "", "Path to a key file (PEM/OpenSSH) or symmetric secret used to protect the agent")
	cmd.Flags().BoolVar(&flags.encrypt, "encrypt", false, "Embed an encrypted copy of the agent instead of a signature (requires --key)")

	return cmd
}

func (f *pushFlags) protection() (oci.PackageOption, error) {
	if f.keyFile == "" {
		if f.encrypt {
			return nil, errors.New("--encrypt requires --key")
		}
		return nil, nil
	}
	key, err := protect.LoadKey(f.keyFile)
	if err != nil {
		return nil, err
	}
	mode := protect.ModeSign
	if f.encrypt {
		mode = protect.ModeEncrypt
	}
	switch {
	case mode == protect.ModeSign && !key.CanSign():
		return nil, fmt.Errorf("%s (%s) cannot sign: use a private Ed25519/ECDSA/RSA key or a secret, or pass --encrypt", f.keyFile, key.Describe())
	case mode == protect.ModeEncrypt && !key.CanEncrypt():
		return nil, fmt.Errorf("%s (%s) cannot encrypt: use an X25519/ECDSA/RSA key or a secret", f.keyFile, key.Describe())
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

	agentSource, err := config.Resolve(agentFilename, nil)
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
