package root

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/evaluation"
)

func sizeRun(pass bool) *evaluation.EvalRun {
	r := evaluation.Result{InputPath: "a", SizeExpected: "medium", Size: "medium"}
	if !pass {
		r.Size = "short"
	}
	return &evaluation.EvalRun{Name: "run", Results: []evaluation.Result{r}}
}

func TestCheckBaseline_NoBaselineIsANoOp(t *testing.T) {
	t.Parallel()

	f := &evalFlags{}
	var buf bytes.Buffer
	require.NoError(t, f.checkBaseline(&buf, sizeRun(false)))
	assert.Empty(t, buf.String(), "without --baseline nothing is compared or printed")
}

func TestCheckBaseline_RegressionReturnsAnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path, err := evaluation.SaveRunJSON(sizeRun(true), dir)
	require.NoError(t, err)

	f := &evalFlags{baseline: path}
	var buf bytes.Buffer
	err = f.checkBaseline(&buf, sizeRun(false))

	require.Error(t, err, "a regression must surface as a non-zero exit")
	assert.Contains(t, err.Error(), "regressed against baseline")
	assert.Contains(t, buf.String(), "Regression against baseline")
}

func TestCheckBaseline_NoRegressionSucceeds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path, err := evaluation.SaveRunJSON(sizeRun(true), dir)
	require.NoError(t, err)

	f := &evalFlags{baseline: path}
	var buf bytes.Buffer
	require.NoError(t, f.checkBaseline(&buf, sizeRun(true)))
	assert.Contains(t, buf.String(), "No regression against baseline")
}

func TestCheckBaseline_ToleranceIsPlumbedThrough(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	base := &evaluation.EvalRun{Name: "b", Results: []evaluation.Result{
		{InputPath: "a", ToolCallsExpected: 0.8, ToolCallsScore: 1.0},
		{InputPath: "b", ToolCallsExpected: 0.8, ToolCallsScore: 1.0},
	}}
	cur := &evaluation.EvalRun{Name: "c", Results: []evaluation.Result{
		{InputPath: "a", ToolCallsExpected: 0.8, ToolCallsScore: 1.0},
		{InputPath: "b", ToolCallsExpected: 0.8, ToolCallsScore: 0.9},
	}}

	path, err := evaluation.SaveRunJSON(base, dir)
	require.NoError(t, err)

	var buf bytes.Buffer
	strict := &evalFlags{baseline: path, regressionTolerance: 0.01}
	require.Error(t, strict.checkBaseline(&buf, cur), "a tight tolerance gates the 0.05 drop")

	buf.Reset()
	lenient := &evalFlags{baseline: path, regressionTolerance: 0.10}
	require.NoError(t, lenient.checkBaseline(&buf, cur), "a wider tolerance absorbs it")
}

func TestCheckBaseline_MissingBaselineFileIsAnError(t *testing.T) {
	t.Parallel()

	f := &evalFlags{baseline: filepath.Join(t.TempDir(), "nope.json")}
	var buf bytes.Buffer
	err := f.checkBaseline(&buf, sizeRun(true))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading baseline",
		"a bad --baseline path must fail loudly rather than silently skipping the gate")
}

func TestEvalCmd_BaselineFlagsAreRegistered(t *testing.T) {
	t.Parallel()

	cmd := newEvalCmd()
	require.NotNil(t, cmd.Flags().Lookup("baseline"))

	tolerance := cmd.Flags().Lookup("regression-tolerance")
	require.NotNil(t, tolerance)
	assert.Equal(t, "0", tolerance.DefValue, "the default gates any drop")
}
