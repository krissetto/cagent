package evaluation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// verifyResult holds the outcome of running a verify script inside the
// evaluation container.
type verifyResult struct {
	Passed   bool
	ExitCode int
	Output   string
}

// runVerifyScript executes a shell verify script and returns its outcome.
// The script is run with sh -c; a zero exit code means pass. Output is
// capped at maxVerifyOutputBytes to avoid unbounded memory from chatty
// scripts.
func runVerifyScript(ctx context.Context, script, containerRuntime, containerName string) (verifyResult, error) {
	if script == "" {
		return verifyResult{Passed: true}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	args := []string{"exec", containerName, "sh", "-c", script}
	cmd := exec.CommandContext(ctx, containerRuntime, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	output := strings.TrimSpace(stdout.String())
	if errOutput := strings.TrimSpace(stderr.String()); errOutput != "" {
		if output != "" {
			output += "\n"
		}
		output += errOutput
	}

	// Cap output to avoid unbounded memory.
	if len(output) > maxVerifyOutputBytes {
		output = output[:maxVerifyOutputBytes] + "...(truncated)"
	}

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return verifyResult{ExitCode: -1, Output: output}, fmt.Errorf("running verify script: %w", err)
		}
	}

	slog.DebugContext(ctx, "Verify script completed",
		"exit_code", exitCode,
		"output_length", len(output),
	)

	return verifyResult{
		Passed:   exitCode == 0,
		ExitCode: exitCode,
		Output:   output,
	}, nil
}

// maxVerifyOutputBytes caps the output captured from a verify script.
const maxVerifyOutputBytes = 8192
