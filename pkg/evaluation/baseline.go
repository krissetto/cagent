package evaluation

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"
)

// Metrics is the comparable shape of an evaluation run: the rates a regression
// gate can be built on, derived from the same fields [computeSummary] uses.
//
// Rates are 0 when their denominator is 0, and the corresponding Has… flag says
// whether the rate means anything. Without that distinction "no size
// expectations declared" and "every size expectation failed" would both read as
// 0.0 and a gate could not tell them apart.
type Metrics struct {
	TotalEvals  int     `json:"total_evals"`
	FailedEvals int     `json:"failed_evals"`
	FailureRate float64 `json:"failure_rate"`

	SizePassRate float64 `json:"size_pass_rate"`
	HasSizes     bool    `json:"has_sizes"`

	ToolsF1Mean float64 `json:"tools_f1_mean"`
	HasTools    bool    `json:"has_tools"`

	RelevanceRate float64 `json:"relevance_rate"`
	HasRelevance  bool    `json:"has_relevance"`

	TotalCost float64 `json:"total_cost"`
}

// MetricsOf derives [Metrics] from a run's results.
func MetricsOf(run *EvalRun) Metrics {
	if run == nil {
		return Metrics{}
	}
	s := computeSummary(run.Results)

	m := Metrics{
		TotalEvals:  s.TotalEvals,
		FailedEvals: s.FailedEvals,
		TotalCost:   s.TotalCost,
	}
	if s.TotalEvals > 0 {
		m.FailureRate = float64(s.FailedEvals) / float64(s.TotalEvals)
	}
	if s.SizesTotal > 0 {
		m.HasSizes = true
		m.SizePassRate = float64(s.SizesPassed) / float64(s.SizesTotal)
	}
	if s.ToolsCount > 0 {
		m.HasTools = true
		m.ToolsF1Mean = s.ToolsF1Sum / float64(s.ToolsCount)
	}
	if s.RelevanceTotal > 0 {
		m.HasRelevance = true
		m.RelevanceRate = s.RelevancePassed / s.RelevanceTotal
	}
	return m
}

// MetricDelta is one metric's movement between two runs. Higher is better for
// quality rates and worse for FailureRate, so Regressed — not the sign of Delta
// — is what a gate reads.
type MetricDelta struct {
	Name      string  `json:"name"`
	Baseline  float64 `json:"baseline"`
	Current   float64 `json:"current"`
	Delta     float64 `json:"delta"`
	Regressed bool    `json:"regressed"`
	// Informational marks a metric that is reported but never gates, so a
	// reviewer can see it moved without the build failing over it.
	Informational bool `json:"informational,omitempty"`
}

// EvalChange records one evaluation's pass/fail transition between runs.
type EvalChange struct {
	InputPath string `json:"input_path"`
	// Was and Now are "pass", "fail", or "absent".
	Was       string `json:"was"`
	Now       string `json:"now"`
	Regressed bool   `json:"regressed"`
}

// Comparison is the result of checking a run against a baseline.
type Comparison struct {
	Baseline  Metrics       `json:"baseline"`
	Current   Metrics       `json:"current"`
	Tolerance float64       `json:"tolerance"`
	Deltas    []MetricDelta `json:"deltas"`
	Changes   []EvalChange  `json:"changes"`
	// Regressed is true when any gating metric moved beyond the tolerance, or
	// an evaluation that passed in the baseline now fails.
	Regressed bool `json:"regressed"`
}

// LoadBaseline reads a run previously written by [SaveRunJSON].
func LoadBaseline(path string) (*EvalRun, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading baseline %q: %w", path, err)
	}
	var run EvalRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("parsing baseline %q: %w", path, err)
	}
	return &run, nil
}

// Compare checks current against baseline.
//
// tolerance is the amount an aggregate quality rate may fall (or the failure
// rate may climb) before it counts as a regression, so judge variance does not
// fail a build on noise. A tolerance of 0 means any movement in the wrong
// direction regresses; a negative value is clamped to 0.
//
// The tolerance governs aggregate rates ONLY. An evaluation that passed in the
// baseline and now fails gates regardless of tolerance — that transition is the
// exact signal this check exists to catch, and absorbing it would defeat the
// point. Consequently a large tolerance still cannot hide an outright breakage.
//
// Note the corollary: adding a new FAILING evaluation lowers the aggregate rate
// and therefore gates, even though no existing evaluation regressed. That is
// intended — a suite that got worse should say so — but it means "add a known-
// failing eval as a TODO" needs an explicit tolerance bump or a fix.
//
// Cost is reported but never gates: a cost increase is not a quality regression,
// and gating on it would make the check fire on provider price changes.
//
// A metric absent from either run is skipped rather than treated as 0 — adding
// the first size expectation to a suite must not look like a regression.
func Compare(baseline, current *EvalRun, tolerance float64) Comparison {
	if tolerance < 0 {
		tolerance = 0
	}

	c := Comparison{
		Baseline:  MetricsOf(baseline),
		Current:   MetricsOf(current),
		Tolerance: tolerance,
	}

	// Quality rates: a drop beyond the tolerance regresses.
	for _, q := range []struct {
		name            string
		base, cur       float64
		hasBase, hasCur bool
	}{
		{"size pass rate", c.Baseline.SizePassRate, c.Current.SizePassRate, c.Baseline.HasSizes, c.Current.HasSizes},
		{"tool F1 mean", c.Baseline.ToolsF1Mean, c.Current.ToolsF1Mean, c.Baseline.HasTools, c.Current.HasTools},
		{"relevance rate", c.Baseline.RelevanceRate, c.Current.RelevanceRate, c.Baseline.HasRelevance, c.Current.HasRelevance},
	} {
		if !q.hasBase || !q.hasCur {
			continue
		}
		d := MetricDelta{Name: q.name, Baseline: q.base, Current: q.cur, Delta: q.cur - q.base}
		d.Regressed = q.cur < q.base-tolerance
		c.Deltas = append(c.Deltas, d)
	}

	// Failure rate: a climb beyond the tolerance regresses.
	if c.Baseline.TotalEvals > 0 && c.Current.TotalEvals > 0 {
		d := MetricDelta{
			Name:     "failure rate",
			Baseline: c.Baseline.FailureRate,
			Current:  c.Current.FailureRate,
			Delta:    c.Current.FailureRate - c.Baseline.FailureRate,
		}
		d.Regressed = c.Current.FailureRate > c.Baseline.FailureRate+tolerance
		c.Deltas = append(c.Deltas, d)
	}

	c.Deltas = append(c.Deltas, MetricDelta{
		Name:          "total cost",
		Baseline:      c.Baseline.TotalCost,
		Current:       c.Current.TotalCost,
		Delta:         c.Current.TotalCost - c.Baseline.TotalCost,
		Informational: true,
	})

	c.Changes = compareEvals(baseline, current)

	for _, d := range c.Deltas {
		if d.Regressed && !d.Informational {
			c.Regressed = true
		}
	}
	for _, ch := range c.Changes {
		if ch.Regressed {
			c.Regressed = true
		}
	}
	return c
}

// compareEvals pairs evaluations by input path and reports transitions. Only
// pass → fail is a regression; appearing and disappearing evaluations are
// reported so a reviewer notices a suite change, but do not gate.
func compareEvals(baseline, current *EvalRun) []EvalChange {
	was := passByPath(baseline)
	now := passByPath(current)

	paths := make(map[string]struct{}, len(was)+len(now))
	for p := range was {
		paths[p] = struct{}{}
	}
	for p := range now {
		paths[p] = struct{}{}
	}

	changes := make([]EvalChange, 0, len(paths))
	for p := range paths {
		bs, inBase := was[p]
		cs, inCur := now[p]
		change := EvalChange{
			InputPath: p,
			Was:       passLabel(bs, inBase),
			Now:       passLabel(cs, inCur),
		}
		if change.Was == change.Now {
			continue
		}
		change.Regressed = inBase && bs && inCur && !cs
		changes = append(changes, change)
	}

	slices.SortFunc(changes, func(a, b EvalChange) int {
		// Regressions first, then alphabetical, so the important lines lead.
		if a.Regressed != b.Regressed {
			if a.Regressed {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.InputPath, b.InputPath)
	})
	return changes
}

func passByPath(run *EvalRun) map[string]bool {
	out := map[string]bool{}
	if run == nil {
		return out
	}
	for _, r := range run.Results {
		// Repeated runs of the same input (Config.Repeat > 1) collapse
		// pessimistically: if any repetition failed, the eval counts as failed.
		if prev, seen := out[r.InputPath]; seen {
			out[r.InputPath] = prev && resultPassed(r)
			continue
		}
		out[r.InputPath] = resultPassed(r)
	}
	return out
}

func passLabel(passed, present bool) string {
	switch {
	case !present:
		return "absent"
	case passed:
		return "pass"
	default:
		return "fail"
	}
}

// resultPassed reports whether one result met every expectation declared for it,
// mirroring the fields [computeSummary] scores. An expectation that was not
// declared is not a failure.
func resultPassed(r Result) bool {
	if r.Error != "" {
		return false
	}
	if r.SizeExpected != "" && r.SizeExpected != r.Size {
		return false
	}
	if r.ToolCallsExpected > 0 && r.ToolCallsScore < r.ToolCallsExpected {
		return false
	}
	if r.RelevanceExpected > 0 && r.RelevancePassed < r.RelevanceExpected {
		return false
	}
	return true
}

// PrintComparison writes a human-readable comparison.
func PrintComparison(out io.Writer, c Comparison) {
	fmt.Fprintf(out, "\nBaseline comparison (tolerance %.3f)\n", c.Tolerance)

	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tBASELINE\tCURRENT\tDELTA\t")
	for _, d := range c.Deltas {
		marker := "  "
		switch {
		case d.Regressed:
			marker = "! "
		case d.Informational:
			marker = "· "
		}
		fmt.Fprintf(tw, "%s%s\t%.3f\t%.3f\t%+.3f\t\n", marker, d.Name, d.Baseline, d.Current, d.Delta)
	}
	_ = tw.Flush()

	if len(c.Changes) > 0 {
		fmt.Fprintln(out, "\nChanged evaluations")
		ctw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		for _, ch := range c.Changes {
			marker := "  "
			if ch.Regressed {
				marker = "! "
			}
			fmt.Fprintf(ctw, "%s%s\t%s → %s\t\n", marker, ch.InputPath, ch.Was, ch.Now)
		}
		_ = ctw.Flush()
	}

	if c.Regressed {
		fmt.Fprintln(out, "\n❌ Regression against baseline")
		return
	}
	fmt.Fprintln(out, "\n✅ No regression against baseline")
}
