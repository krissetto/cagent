package builtin

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestNewShellTool(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	assert.NotNil(t, tool)
	assert.NotNil(t, tool.handler)
	assert.Equal(t, "/bin/bash", tool.handler.shell)

	t.Setenv("SHELL", "")
	tool = NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	assert.NotNil(t, tool)
	assert.NotNil(t, tool.handler)
	assert.Equal(t, "/bin/sh", tool.handler.shell, "Should default to /bin/sh when SHELL is not set")
}

func TestShellTool_HandlerEcho(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{
		Cmd: "echo 'hello world'",
		Cwd: "",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "hello world")
}

func TestShellTool_HandlerWithCwd(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	tmpDir := t.TempDir()

	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{
		Cmd: "pwd",
		Cwd: tmpDir,
	})
	require.NoError(t, err)
	// The output might contain extra newlines or other characters,
	// so we just check if it contains the temp dir path
	assert.Contains(t, result.Output, tmpDir)
}

func TestShellTool_HandlerError(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{
		Cmd: "command_that_does_not_exist",
		Cwd: "",
	})
	require.NoError(t, err, "Handler should not return an error")
	assert.Contains(t, result.Output, "Error executing command")
}

func TestShellTool_OutputSchema(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		assert.NotNil(t, tool.OutputSchema)
	}
}

func TestShellTool_ParametersAreObjects(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		m, err := tools.SchemaToMap(tool.Parameters)
		require.NoError(t, err)
		assert.Equal(t, "object", m["type"])
	}
}

// Minimal tests for background job features
func TestShellTool_RunBackgroundJob(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	err := tool.Start(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = tool.Stop(t.Context())
	})

	result, err := tool.handler.RunShellBackground(t.Context(), RunShellBackgroundArgs{Cmd: "echo test"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Background job started with ID:")
}

func TestShellTool_ListBackgroundJobs(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	err := tool.Start(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = tool.Stop(t.Context())
	})

	// Start a background job first
	_, err = tool.handler.RunShellBackground(t.Context(), RunShellBackgroundArgs{Cmd: "echo test"})
	require.NoError(t, err)

	// No need to wait - ListBackgroundJobs shows jobs regardless of status
	listResult, err := tool.handler.ListBackgroundJobs(t.Context(), nil)

	require.NoError(t, err)
	assert.Contains(t, listResult.Output, "Background Jobs:")
	assert.Contains(t, listResult.Output, "ID: job_")
}

func TestShellTool_ViewBackgroundJob_NotFound(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	result, err := tool.handler.ViewBackgroundJob(t.Context(), ViewBackgroundJobArgs{JobID: "nonexistent"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Job not found")
}

func TestShellTool_StopBackgroundJob_NotFound(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	result, err := tool.handler.StopBackgroundJob(t.Context(), StopBackgroundJobArgs{JobID: "nonexistent"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Job not found")
}

func TestShellTool_StopBackgroundJob_StopsRunningJob(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	startResult, err := tool.handler.RunShellBackground(t.Context(), RunShellBackgroundArgs{Cmd: "sleep 60"})
	require.NoError(t, err)

	// Extract job ID from result
	output := startResult.Output
	jobIDStart := strings.Index(output, "job_")
	require.Greater(t, jobIDStart, -1, "should contain job ID")
	jobIDEnd := strings.IndexByte(output[jobIDStart:], '\n')
	var jobID string
	if jobIDEnd == -1 {
		jobID = output[jobIDStart:]
	} else {
		jobID = output[jobIDStart : jobIDStart+jobIDEnd]
	}
	jobID = strings.TrimSpace(jobID)

	stopResult, err := tool.handler.StopBackgroundJob(t.Context(), StopBackgroundJobArgs{JobID: jobID})
	require.NoError(t, err)
	assert.Contains(t, stopResult.Output, "stopped successfully")

	// Verify status changed
	viewResult, err := tool.handler.ViewBackgroundJob(t.Context(), ViewBackgroundJobArgs{JobID: jobID})
	require.NoError(t, err)
	assert.Contains(t, viewResult.Output, "stopped")
}

func TestShellTool_ViewBackgroundJob_CompletedShowsOutput(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	startResult, err := tool.handler.RunShellBackground(t.Context(), RunShellBackgroundArgs{Cmd: `echo "hello from bg"`})
	require.NoError(t, err)

	output := startResult.Output
	jobIDStart := strings.Index(output, "job_")
	require.Greater(t, jobIDStart, -1)
	jobIDEnd := strings.IndexByte(output[jobIDStart:], '\n')
	var jobID string
	if jobIDEnd == -1 {
		jobID = output[jobIDStart:]
	} else {
		jobID = output[jobIDStart : jobIDStart+jobIDEnd]
	}
	jobID = strings.TrimSpace(jobID)

	// Wait for job to complete
	require.Eventually(t, func() bool {
		viewResult, err := tool.handler.ViewBackgroundJob(t.Context(), ViewBackgroundJobArgs{JobID: jobID})
		return err == nil && (strings.Contains(viewResult.Output, "completed") || strings.Contains(viewResult.Output, "failed"))
	}, 5*time.Second, 100*time.Millisecond)

	viewResult, err := tool.handler.ViewBackgroundJob(t.Context(), ViewBackgroundJobArgs{JobID: jobID})
	require.NoError(t, err)
	assert.Contains(t, viewResult.Output, "completed")
	assert.Contains(t, viewResult.Output, "hello from bg")
	assert.Contains(t, viewResult.Output, "Exit Code: 0")
}

func TestShellTool_ViewBackgroundJob_FailedShowsNonZeroExit(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	startResult, err := tool.handler.RunShellBackground(t.Context(), RunShellBackgroundArgs{Cmd: "exit 42"})
	require.NoError(t, err)

	output := startResult.Output
	jobIDStart := strings.Index(output, "job_")
	require.Greater(t, jobIDStart, -1)
	jobIDEnd := strings.IndexByte(output[jobIDStart:], '\n')
	var jobID string
	if jobIDEnd == -1 {
		jobID = output[jobIDStart:]
	} else {
		jobID = output[jobIDStart : jobIDStart+jobIDEnd]
	}
	jobID = strings.TrimSpace(jobID)

	require.Eventually(t, func() bool {
		viewResult, err := tool.handler.ViewBackgroundJob(t.Context(), ViewBackgroundJobArgs{JobID: jobID})
		return err == nil && (strings.Contains(viewResult.Output, "failed") || strings.Contains(viewResult.Output, "completed"))
	}, 5*time.Second, 100*time.Millisecond)

	viewResult, err := tool.handler.ViewBackgroundJob(t.Context(), ViewBackgroundJobArgs{JobID: jobID})
	require.NoError(t, err)
	assert.Contains(t, viewResult.Output, "failed")
	assert.Contains(t, viewResult.Output, "Exit Code: 42")
}

func TestShellTool_Stop_TerminatesRunningJobs(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	require.NoError(t, tool.Start(t.Context()))

	_, err := tool.handler.RunShellBackground(t.Context(), RunShellBackgroundArgs{Cmd: "sleep 60"})
	require.NoError(t, err)

	// Stop should return promptly (well within 10 seconds) and terminate the job
	done := make(chan struct{})
	go func() {
		_ = tool.Stop(t.Context())
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() did not return within 10 seconds")
	}

	// All jobs should be in stopped state
	tool.handler.jobs.Range(func(_ string, job *backgroundJob) bool {
		status := job.status.Load()
		assert.NotEqual(t, statusRunning, status, "job should not still be running after Stop()")
		return true
	})
}

func TestShellTool_StopBackgroundJob_AlreadyStopped(t *testing.T) {
	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	startResult, err := tool.handler.RunShellBackground(t.Context(), RunShellBackgroundArgs{Cmd: `echo done`})
	require.NoError(t, err)

	output := startResult.Output
	jobIDStart := strings.Index(output, "job_")
	require.Greater(t, jobIDStart, -1)
	jobIDEnd := strings.IndexByte(output[jobIDStart:], '\n')
	var jobID string
	if jobIDEnd == -1 {
		jobID = output[jobIDStart:]
	} else {
		jobID = output[jobIDStart : jobIDStart+jobIDEnd]
	}
	jobID = strings.TrimSpace(jobID)

	// Wait for job to finish
	require.Eventually(t, func() bool {
		viewResult, _ := tool.handler.ViewBackgroundJob(t.Context(), ViewBackgroundJobArgs{JobID: jobID})
		return viewResult != nil && (strings.Contains(viewResult.Output, "completed") || strings.Contains(viewResult.Output, "failed"))
	}, 5*time.Second, 100*time.Millisecond)

	// Trying to stop an already-completed job should return an error message
	stopResult, err := tool.handler.StopBackgroundJob(t.Context(), StopBackgroundJobArgs{JobID: jobID})
	require.NoError(t, err)
	assert.Contains(t, stopResult.Output, "not running")
}

func TestShellTool_Instructions(t *testing.T) {
	t.Parallel()

	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	instructions := tool.Instructions()

	// Check that native instructions are returned
	assert.Contains(t, instructions, "Shell Tools")
}

func TestResolveWorkDir(t *testing.T) {
	t.Parallel()

	workingDir := "/configured/project"
	h := &shellHandler{workingDir: workingDir}

	tests := []struct {
		name     string
		cwd      string
		expected string
	}{
		{name: "empty defaults to workingDir", cwd: "", expected: workingDir},
		{name: "dot defaults to workingDir", cwd: ".", expected: workingDir},
		{name: "absolute path unchanged", cwd: "/tmp/other", expected: "/tmp/other"},
		{name: "relative path joined with workingDir", cwd: "src/pkg", expected: "/configured/project/src/pkg"},
		{name: "relative with dot prefix", cwd: "./subdir", expected: "/configured/project/subdir"},
		{name: "relative with parent traversal", cwd: "../sibling", expected: "/configured/sibling"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, h.resolveWorkDir(tt.cwd))
		})
	}
}

func TestShellTool_RelativeCwdResolvesAgainstWorkingDir(t *testing.T) {
	// Create a directory structure: workingDir/subdir/
	workingDir := t.TempDir()
	subdir := workingDir + "/subdir"
	require.NoError(t, os.Mkdir(subdir, 0o755))

	tool := NewShellTool(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: workingDir}})

	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{
		Cmd: "pwd",
		Cwd: "subdir",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, subdir,
		"relative cwd must resolve against the configured workingDir, not the process cwd")
}
