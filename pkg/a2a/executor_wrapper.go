package a2a

import (
	"context"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
	"google.golang.org/adk/v2/server/adka2a"
)

// executorWrapper wraps an ADK executor and fixes artifact update events
// to ensure they have non-nil Parts slices, which is required by the A2A spec.
type executorWrapper struct {
	executor *adka2a.Executor
}

func newExecutorWrapper(config adka2a.ExecutorConfig) *executorWrapper {
	return &executorWrapper{
		executor: adka2a.NewExecutor(config),
	}
}

func (w *executorWrapper) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	// Create a wrapping queue that fixes events before sending them
	fixedQueue := &fixingQueue{
		queue: queue,
	}
	return w.executor.Execute(ctx, reqCtx, fixedQueue)
}

func (w *executorWrapper) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, queue eventqueue.Queue) error {
	return w.executor.Cancel(ctx, reqCtx, queue)
}

// fixingQueue wraps an eventqueue.Queue and fixes artifact update events
type fixingQueue struct {
	queue eventqueue.Queue
}

func (fq *fixingQueue) Write(ctx context.Context, event a2a.Event) error {
	// Fix artifact update events with nil Parts
	if artifactEvent, ok := event.(*a2a.TaskArtifactUpdateEvent); ok {
		if artifactEvent.Artifact != nil && artifactEvent.Artifact.Parts == nil {
			// Replace nil with an empty slice
			artifactEvent.Artifact.Parts = []a2a.Part{}
		}
	}
	return fq.queue.Write(ctx, event)
}

func (fq *fixingQueue) Read(ctx context.Context) (a2a.Event, a2a.TaskVersion, error) {
	return fq.queue.Read(ctx)
}

func (fq *fixingQueue) WriteVersioned(ctx context.Context, event a2a.Event, version a2a.TaskVersion) error {
	return fq.queue.WriteVersioned(ctx, event, version)
}

func (fq *fixingQueue) Close() error {
	return fq.queue.Close()
}
