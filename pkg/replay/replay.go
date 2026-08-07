// Package replay compares the behaviour of two recorded agent sessions.
//
// It answers the triage question "did the agent do something different this
// time, and where did it first diverge?" — the thing you actually want to know
// when a task that worked yesterday does not work today.
//
// # Behaviour, not prose
//
// Comparison is over the sequence of tool calls, not over the assistant's text.
// Model output is nondeterministic: two runs of the same task almost always word
// things differently while doing exactly the same work, so diffing prose reports
// a difference on essentially every comparison and is useless as a signal. The
// tool calls are what changed the world, so they are what is compared.
//
// Assistant text is still carried on each [Turn] so a reporter can show what was
// said around a divergence; it just does not decide whether a divergence
// happened.
//
// # First divergence only
//
// Once two runs differ, everything after that point is downstream of the
// difference and comparing it produces noise, not information. So the comparison
// stops at the first divergence and reports where it was.
package replay

import (
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
)

// ToolCall is the comparable part of a tool invocation.
type ToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Turn is one assistant turn: what it said, and what it called.
type Turn struct {
	// Index is the turn's 0-based position in the session.
	Index int `json:"index"`
	// Agent is the agent that produced the turn, so a divergence in a
	// multi-agent run can be attributed.
	Agent string `json:"agent,omitempty"`
	// Content is the assistant text. Carried for reporting; never compared.
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Kind classifies how two runs differ.
type Kind string

const (
	// KindToolCalls means both runs made a turn at this index but called
	// different tools, or the same tools with different arguments.
	KindToolCalls Kind = "tool_calls"
	// KindExtraTurn means the second run kept going after the first stopped.
	KindExtraTurn Kind = "extra_turn"
	// KindMissingTurn means the second run stopped before the first did.
	KindMissingTurn Kind = "missing_turn"
)

// Divergence is the first behavioural difference found.
type Divergence struct {
	Kind Kind `json:"kind"`
	// TurnIndex is the 0-based turn at which the runs first differ.
	TurnIndex int `json:"turn_index"`
	// A and B are the diverging turns. Either may be nil when one run had no
	// turn at this index.
	A *Turn `json:"a,omitempty"`
	B *Turn `json:"b,omitempty"`
}

// Result is the outcome of a comparison.
type Result struct {
	TurnsA int `json:"turns_a"`
	TurnsB int `json:"turns_b"`
	// TurnsMatched is how many leading turns behaved identically.
	TurnsMatched int `json:"turns_matched"`
	// Divergence is nil when both runs behaved identically throughout.
	Divergence *Divergence `json:"divergence,omitempty"`
}

// Identical reports whether the two runs behaved the same the whole way.
func (r Result) Identical() bool { return r.Divergence == nil }

// TurnsOf extracts the assistant turns from a session, in order.
//
// Only assistant messages are turns: user messages are inputs and tool messages
// are results, neither of which is a decision the model made. A nil session
// yields no turns.
func TurnsOf(sess *session.Session) []Turn {
	if sess == nil {
		return nil
	}

	var turns []Turn
	for i := range sess.Messages {
		item := &sess.Messages[i]
		if item.Message == nil {
			continue
		}
		msg := &item.Message.Message
		if msg.Role != "assistant" {
			continue
		}

		turn := Turn{
			Index:   len(turns),
			Agent:   item.Message.AgentName,
			Content: msg.Content,
		}
		for _, tc := range msg.ToolCalls {
			turn.ToolCalls = append(turn.ToolCalls, ToolCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		turns = append(turns, turn)
	}
	return turns
}

// Compare walks two runs' turns and reports the first behavioural divergence.
func Compare(a, b []Turn) Result {
	result := Result{TurnsA: len(a), TurnsB: len(b)}

	shorter := min(len(a), len(b))
	for i := range shorter {
		if sameBehaviour(a[i], b[i]) {
			result.TurnsMatched++
			continue
		}
		result.Divergence = &Divergence{
			Kind:      KindToolCalls,
			TurnIndex: i,
			A:         &a[i],
			B:         &b[i],
		}
		return result
	}

	// The common prefix behaved identically; one run may have continued.
	switch {
	case len(b) > len(a):
		result.Divergence = &Divergence{Kind: KindExtraTurn, TurnIndex: shorter, B: &b[shorter]}
	case len(a) > len(b):
		result.Divergence = &Divergence{Kind: KindMissingTurn, TurnIndex: shorter, A: &a[shorter]}
	}
	return result
}

// CompareSessions is TurnsOf on both sides followed by Compare.
func CompareSessions(a, b *session.Session) Result {
	return Compare(TurnsOf(a), TurnsOf(b))
}

// sameBehaviour reports whether two turns called the same tools with the same
// arguments, in the same order. Order matters: calling a tool before another is
// a different plan, even with the same set of calls.
func sameBehaviour(a, b Turn) bool {
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i] != b.ToolCalls[i] {
			return false
		}
	}
	return true
}

// PrintResult writes a human-readable comparison.
func PrintResult(out io.Writer, r Result, nameA, nameB string) {
	fmt.Fprintf(out, "Comparing %s (%d turns) against %s (%d turns)\n", nameA, r.TurnsA, nameB, r.TurnsB)

	if r.Identical() {
		fmt.Fprintf(out, "\n✅ Identical behaviour across all %d turns.\n", r.TurnsMatched)
		return
	}

	d := r.Divergence
	fmt.Fprintf(out, "\n❌ First divergence at turn %d (after %d matching turn(s)).\n", d.TurnIndex, r.TurnsMatched)

	switch d.Kind {
	case KindExtraTurn:
		fmt.Fprintf(out, "   %s stopped; %s continued with:\n", nameA, nameB)
		printCalls(out, d.B)
	case KindMissingTurn:
		fmt.Fprintf(out, "   %s continued; %s stopped. %s did:\n", nameA, nameB, nameA)
		printCalls(out, d.A)
	case KindToolCalls:
		fmt.Fprintf(out, "   %s called:\n", nameA)
		printCalls(out, d.A)
		fmt.Fprintf(out, "   %s called:\n", nameB)
		printCalls(out, d.B)
	}

	fmt.Fprintln(out, "\nEverything after this point is downstream of the divergence and is not compared.")
}

func printCalls(out io.Writer, t *Turn) {
	if t == nil {
		return
	}
	if len(t.ToolCalls) == 0 {
		fmt.Fprintln(out, "     (no tool calls — a final answer)")
		return
	}
	for _, tc := range t.ToolCalls {
		fmt.Fprintf(out, "     %s(%s)\n", tc.Name, truncateArgs(tc.Arguments))
	}
}

// truncateArgs bounds argument text so a large payload cannot flood the report.
func truncateArgs(args string) string {
	const maxRunes = 120
	args = strings.ReplaceAll(args, "\n", " ")
	runes := []rune(args)
	if len(runes) <= maxRunes {
		return args
	}
	return string(runes[:maxRunes-1]) + "…"
}
