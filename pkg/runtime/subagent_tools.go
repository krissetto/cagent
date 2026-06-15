package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/subagents"
)

const (
	ToolNameSubagentStart    = subagents.ToolNameSubagentStart
	ToolNameSubagentSend     = subagents.ToolNameSubagentSend
	ToolNameSubagentInspect  = subagents.ToolNameSubagentInspect
	ToolNameSubagentList     = subagents.ToolNameSubagentList
	ToolNameSubagentStop     = subagents.ToolNameSubagentStop
	ToolNameSubagentFinalize = subagents.ToolNameSubagentFinalize
	ToolNameSubagentClose    = "subagent_close"
)

type subagentStartArgs struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}
type subagentRefArgs struct {
	SubagentID string `json:"subagent_id"`
}
type subagentSendArgs struct {
	SubagentID string `json:"subagent_id"`
	Message    string `json:"message"`
}
type subagentInspectArgs struct {
	SubagentID string `json:"subagent_id"`
	Mode       string `json:"mode"`
}

func (r *LocalRuntime) registerSubagentTools() {
	for _, name := range []string{ToolNameSubagentStart, ToolNameSubagentSend, ToolNameSubagentInspect, ToolNameSubagentList, ToolNameSubagentStop, ToolNameSubagentFinalize, ToolNameSubagentClose} {
		r.toolMap[name] = func(ctx context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
			return r.handleSubagentTool(ctx, sess, tc.Function.Name, tc)
		}
	}
}

func (r *LocalRuntime) handleSubagentTool(ctx context.Context, sess *session.Session, name string, tc tools.ToolCall) (*tools.ToolCallResult, error) {
	if r.subagents == nil {
		return tools.ResultError("subagent manager unavailable"), nil
	}
	switch name {
	case ToolNameSubagentStart:
		var args subagentStartArgs
		if err := decodeToolArgs(tc, &args); err != nil {
			return nil, err
		}
		h, err := r.subagents.Start(ctx, sess, args.Agent, args.Task)
		if err != nil {
			return tools.ResultError(err.Error()), nil
		}
		return jsonResult(h.info()), nil
	case ToolNameSubagentSend:
		var args subagentSendArgs
		if err := decodeToolArgs(tc, &args); err != nil {
			return nil, err
		}
		if err := r.subagents.Send(sess, args.SubagentID, args.Message); err != nil {
			return tools.ResultError(err.Error()), nil
		}
		return tools.ResultSuccess("sent"), nil
	case ToolNameSubagentInspect:
		var args subagentInspectArgs
		if err := decodeToolArgs(tc, &args); err != nil {
			return nil, err
		}
		out, err := r.subagents.Inspect(sess, args.SubagentID, args.Mode)
		if err != nil {
			return tools.ResultError(err.Error()), nil
		}
		return tools.ResultSuccess(out), nil
	case ToolNameSubagentList:
		return jsonResult(r.subagents.List(sess)), nil
	case ToolNameSubagentStop:
		var args subagentRefArgs
		if err := decodeToolArgs(tc, &args); err != nil {
			return nil, err
		}
		if err := r.subagents.Stop(sess, args.SubagentID); err != nil {
			return tools.ResultError(err.Error()), nil
		}
		return tools.ResultSuccess("stopped"), nil
	case ToolNameSubagentFinalize, ToolNameSubagentClose:
		var args subagentRefArgs
		if err := decodeToolArgs(tc, &args); err != nil {
			return nil, err
		}
		if err := r.subagents.Finalize(sess, args.SubagentID); err != nil {
			return tools.ResultError(err.Error()), nil
		}
		return tools.ResultSuccess("finalized"), nil
	default:
		return nil, errors.New("unknown subagent tool")
	}
}

func decodeToolArgs(tc tools.ToolCall, dest any) error {
	args := strings.TrimSpace(tc.Function.Arguments)
	if args == "" {
		args = "{}"
	}
	return json.Unmarshal([]byte(args), dest)
}
