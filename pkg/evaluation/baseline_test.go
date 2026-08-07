package evaluation

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sizeResult is a result whose only declared expectation is response size.
func sizeResult(path string, pass bool) Result {
	r := Result{InputPath: path, SizeExpected: "medium", Size: "medium"}
	if !pass {
		r.Size = "short"
	}
	return r
}

func run(results ...Result) *EvalRun {
	return &EvalRun{Name: "run", Results: results}
}

func TestMetricsOf_RatesAndFlags(t *testing.T) {
	t.Parallel()

	got := MetricsOf(run(
		sizeResult("a", true),
		sizeResult("b", false),
		Result{InputPath: "c", Error: "boom"},
		Result{InputPath: "d", ToolCallsExpected: 1, ToolCallsScore: 0.5, Cost: 0.25},
		Result{InputPath: "e", RelevanceExpected: 2, RelevancePassed: 1},
	))

	assert.Equal(t, 5, got.TotalEvals)
	assert.Equal(t, 1, got.FailedEvals)
	assert.InDelta(t, 0.2, got.FailureRate, 1e-9)
	assert.True(t, got.HasSizes)
	assert.InDelta(t, 0.5, got.SizePassRate, 1e-9)
	assert.True(t, got.HasTools)
	assert.InDelta(t, 0.5, got.ToolsF1Mean, 1e-9)
	assert.True(t, got.HasRelevance)
	assert.InDelta(t, 0.5, got.RelevanceRate, 1e-9)
	assert.InDelta(t, 0.25, got.TotalCost, 1e-9)
}

// A rate with no denominator must be distinguishable from a rate of 0.0, or a
// gate cannot tell "nothing declared" from "everything failed".
func TestMetricsOf_AbsentMetricsAreFlaggedNotZero(t *testing.T) {
	t.Parallel()

	got := MetricsOf(run(Result{InputPath: "a"}))
	assert.False(t, got.HasSizes)
	assert.False(t, got.HasTools)
	assert.False(t, got.HasRelevance)
	assert.Zero(t, got.SizePassRate)

	assert.Equal(t, Metrics{}, MetricsOf(nil), "a nil run yields zero metrics, not a panic")
}

func TestCompare_NoChangeIsNotARegression(t *testing.T) {
	t.Parallel()

	base := run(sizeResult("a", true), sizeResult("b", true))
	cur := run(sizeResult("a", true), sizeResult("b", true))

	got := Compare(base, cur, 0)
	assert.False(t, got.Regressed)
	assert.Empty(t, got.Changes)
}

func TestCompare_QualityDropRegresses(t *testing.T) {
	t.Parallel()

	base := run(sizeResult("a", true), sizeResult("b", true))
	cur := run(sizeResult("a", true), sizeResult("b", false))

	got := Compare(base, cur, 0)
	require.True(t, got.Regressed)

	var sizeDelta *MetricDelta
	for i := range got.Deltas {
		if got.Deltas[i].Name == "size pass rate" {
			sizeDelta = &got.Deltas[i]
		}
	}
	require.NotNil(t, sizeDelta)
	assert.True(t, sizeDelta.Regressed)
	assert.InDelta(t, -0.5, sizeDelta.Delta, 1e-9)
}

// Judge variance shows up as small movement in the aggregate rates; the
// tolerance exists so a build is not failed by that noise.
//
// Both evaluations stay above their declared expectation (0.8) here, so no
// pass/fail transition occurs and the tolerance is what decides — see
// TestCompare_PassToFailGatesRegardlessOfTolerance for the other half.
func TestCompare_ToleranceAbsorbsSmallDrops(t *testing.T) {
	t.Parallel()

	base := run(
		Result{InputPath: "a", ToolCallsExpected: 0.8, ToolCallsScore: 1.0},
		Result{InputPath: "b", ToolCallsExpected: 0.8, ToolCallsScore: 1.0},
	)
	cur := run(
		Result{InputPath: "a", ToolCallsExpected: 0.8, ToolCallsScore: 1.0},
		Result{InputPath: "b", ToolCallsExpected: 0.8, ToolCallsScore: 0.9},
	)

	// Mean drops 1.00 → 0.95, and both evaluations still pass.
	assert.False(t, Compare(base, cur, 0.10).Regressed, "within tolerance")
	assert.True(t, Compare(base, cur, 0.01).Regressed, "beyond tolerance")
	assert.True(t, Compare(base, cur, 0).Regressed, "zero tolerance gates any drop")
	assert.True(t, Compare(base, cur, -5).Regressed,
		"a negative tolerance is clamped to 0, so this still gates")
}

// The tolerance governs aggregate rates only. An evaluation that passed and now
// fails is the exact signal a regression gate exists to catch, so it gates even
// when the aggregate movement is small enough to be absorbed.
func TestCompare_PassToFailGatesRegardlessOfTolerance(t *testing.T) {
	t.Parallel()

	base := run(
		Result{InputPath: "a", ToolCallsExpected: 1, ToolCallsScore: 1.0},
		Result{InputPath: "b", ToolCallsExpected: 1, ToolCallsScore: 1.0},
	)
	cur := run(
		Result{InputPath: "a", ToolCallsExpected: 1, ToolCallsScore: 1.0},
		Result{InputPath: "b", ToolCallsExpected: 1, ToolCallsScore: 0.9}, // now below expectation
	)

	got := Compare(base, cur, 0.99) // a tolerance far larger than the rate movement
	assert.True(t, got.Regressed, "a pass → fail transition is not absorbed by the tolerance")

	require.Len(t, got.Changes, 1)
	assert.Equal(t, "b", got.Changes[0].InputPath)
	assert.True(t, got.Changes[0].Regressed)
}

func TestCompare_ImprovementIsNotARegression(t *testing.T) {
	t.Parallel()

	base := run(sizeResult("a", false), sizeResult("b", false))
	cur := run(sizeResult("a", true), sizeResult("b", true))

	got := Compare(base, cur, 0)
	assert.False(t, got.Regressed)
	require.NotEmpty(t, got.Changes)
	for _, ch := range got.Changes {
		assert.False(t, ch.Regressed, "fail → pass must never gate")
	}
}

func TestCompare_FailureRateClimbRegresses(t *testing.T) {
	t.Parallel()

	base := run(Result{InputPath: "a"}, Result{InputPath: "b"})
	cur := run(Result{InputPath: "a"}, Result{InputPath: "b", Error: "boom"})

	got := Compare(base, cur, 0)
	assert.True(t, got.Regressed)
}

// Cost is reported but must never gate: a price change is not a quality change.
func TestCompare_CostIsInformationalOnly(t *testing.T) {
	t.Parallel()

	base := run(Result{InputPath: "a", Cost: 0.01})
	cur := run(Result{InputPath: "a", Cost: 100})

	got := Compare(base, cur, 0)
	assert.False(t, got.Regressed, "a cost increase alone must not fail the gate")

	var cost *MetricDelta
	for i := range got.Deltas {
		if got.Deltas[i].Name == "total cost" {
			cost = &got.Deltas[i]
		}
	}
	require.NotNil(t, cost)
	assert.True(t, cost.Informational)
	assert.InDelta(t, 99.99, cost.Delta, 1e-6)
}

// Adding the first expectation of a kind must not read as a regression from 0.
func TestCompare_MetricAbsentFromOneSideIsSkipped(t *testing.T) {
	t.Parallel()

	base := run(Result{InputPath: "a"}) // no size expectations
	cur := run(sizeResult("a", false))  // size expectation newly added, failing

	got := Compare(base, cur, 0)
	for _, d := range got.Deltas {
		assert.NotEqual(t, "size pass rate", d.Name,
			"a metric with no baseline must be skipped, not compared against 0")
	}
}

// An evaluation appearing or disappearing is a suite change, not a regression of
// existing behaviour, so the per-eval transition itself never gates.
func TestCompare_AddedAndRemovedEvalsDoNotGateOnTheirOwn(t *testing.T) {
	t.Parallel()

	base := run(sizeResult("a", true), sizeResult("gone", true))
	cur := run(sizeResult("a", true), sizeResult("new", true))

	got := Compare(base, cur, 0)

	byPath := map[string]EvalChange{}
	for _, ch := range got.Changes {
		byPath[ch.InputPath] = ch
	}
	require.Contains(t, byPath, "gone")
	require.Contains(t, byPath, "new")
	assert.Equal(t, "absent", byPath["gone"].Now)
	assert.Equal(t, "absent", byPath["new"].Was)
	assert.False(t, byPath["gone"].Regressed, "a removed eval is a suite change, not a regression")
	assert.False(t, byPath["new"].Regressed, "an added eval is a suite change, not a regression")
	assert.False(t, got.Regressed, "the aggregate rate is unchanged, so nothing gates")
}

// But an added evaluation that FAILS does lower the aggregate rate, and that
// gates — which is the desirable outcome: a suite that got worse should say so,
// even though no individual evaluation went from passing to failing.
func TestCompare_AddedFailingEvalGatesViaTheAggregateRate(t *testing.T) {
	t.Parallel()

	base := run(sizeResult("a", true))
	cur := run(sizeResult("a", true), sizeResult("new", false))

	got := Compare(base, cur, 0)
	assert.True(t, got.Regressed, "size pass rate fell 1.00 → 0.50")

	for _, ch := range got.Changes {
		if ch.InputPath == "new" {
			assert.False(t, ch.Regressed,
				"the gate comes from the aggregate, not from the added eval's transition")
		}
	}
}

func TestCompare_RegressionsAreListedFirst(t *testing.T) {
	t.Parallel()

	base := run(sizeResult("aaa", true), sizeResult("zzz", true))
	cur := run(sizeResult("aaa", true), sizeResult("zzz", false), sizeResult("bbb", true))

	got := Compare(base, cur, 0)
	require.NotEmpty(t, got.Changes)
	assert.Equal(t, "zzz", got.Changes[0].InputPath, "the regression must lead")
	assert.True(t, got.Changes[0].Regressed)
}

// Config.Repeat runs the same input more than once; one bad repetition means the
// evaluation is not reliably passing.
func TestCompare_RepeatedInputCollapsesPessimistically(t *testing.T) {
	t.Parallel()

	base := run(sizeResult("a", true), sizeResult("a", true))
	cur := run(sizeResult("a", true), sizeResult("a", false))

	got := Compare(base, cur, 0)
	require.Len(t, got.Changes, 1)
	assert.Equal(t, "pass", got.Changes[0].Was)
	assert.Equal(t, "fail", got.Changes[0].Now)
	assert.True(t, got.Changes[0].Regressed)
}

func TestLoadBaseline_RoundTripsSaveRunJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	original := run(sizeResult("a", true), Result{InputPath: "b", Error: "boom"})
	path, err := SaveRunJSON(original, dir)
	require.NoError(t, err)

	loaded, err := LoadBaseline(path)
	require.NoError(t, err)
	assert.Equal(t, MetricsOf(original), MetricsOf(loaded),
		"a saved run must be loadable as a baseline with identical metrics")
}

func TestLoadBaseline_Errors(t *testing.T) {
	t.Parallel()

	_, err := LoadBaseline(filepath.Join(t.TempDir(), "missing.json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading baseline")

	bad := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte("{not json"), 0o600))
	_, err = LoadBaseline(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing baseline")
}

func TestPrintComparison(t *testing.T) {
	t.Parallel()

	t.Run("regression", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		PrintComparison(&buf, Compare(
			run(sizeResult("a", true)),
			run(sizeResult("a", false)),
			0,
		))
		out := buf.String()
		assert.Contains(t, out, "Regression against baseline")
		assert.Contains(t, out, "size pass rate")
		assert.Contains(t, out, "! ", "regressed rows are marked")
	})

	t.Run("clean", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		PrintComparison(&buf, Compare(run(sizeResult("a", true)), run(sizeResult("a", true)), 0))
		assert.Contains(t, buf.String(), "No regression against baseline")
	})
}

func TestComparison_IsJSONSerializable(t *testing.T) {
	t.Parallel()

	c := Compare(run(sizeResult("a", true)), run(sizeResult("a", false)), 0.05)
	data, err := json.Marshal(c)
	require.NoError(t, err)

	var round Comparison
	require.NoError(t, json.Unmarshal(data, &round))
	assert.True(t, round.Regressed)
	assert.InDelta(t, 0.05, round.Tolerance, 1e-9)
}

func TestResultPassed(t *testing.T) {
	t.Parallel()

	assert.True(t, resultPassed(Result{InputPath: "a"}), "no expectations declared means nothing to fail")
	assert.False(t, resultPassed(Result{Error: "boom"}))
	assert.True(t, resultPassed(Result{SizeExpected: "short", Size: "short"}))
	assert.False(t, resultPassed(Result{SizeExpected: "short", Size: "long"}))
	assert.True(t, resultPassed(Result{ToolCallsExpected: 0.8, ToolCallsScore: 0.9}))
	assert.False(t, resultPassed(Result{ToolCallsExpected: 0.8, ToolCallsScore: 0.7}))
	assert.True(t, resultPassed(Result{RelevanceExpected: 2, RelevancePassed: 2}))
	assert.False(t, resultPassed(Result{RelevanceExpected: 2, RelevancePassed: 1}))
}
