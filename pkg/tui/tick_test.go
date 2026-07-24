package tui

import "github.com/docker/docker-agent/pkg/tui/animation"

func acceptedRuntimeTick(runtime *animation.Runtime) animation.TickMsg {
	cmd := runtime.EnsureRunning()
	if cmd == nil {
		panic("no active tick")
	}
	msg, ok := runtime.Accept(cmd().(animation.TickMsg))
	if !ok {
		panic("tick rejected")
	}
	return msg
}
