package runtime

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/session"
)

// sessionDriverRegistry owns the per-session actors for a LocalRuntime. A
// driver is created the first time the runtime sees a session object; detached
// notes for not-yet-seen sessions are kept as orphaned messages and adopted by
// the driver when the session appears.
type sessionDriverRegistry struct {
	r *LocalRuntime

	mu      sync.Mutex
	drivers map[string]*sessionDriver
	orphans map[string][]QueuedMessage
}

func newSessionDriverRegistry(r *LocalRuntime) *sessionDriverRegistry {
	return &sessionDriverRegistry{
		r:       r,
		drivers: map[string]*sessionDriver{},
		orphans: map[string][]QueuedMessage{},
	}
}

func (g *sessionDriverRegistry) Get(sess *session.Session) *sessionDriver {
	if sess == nil {
		return nil
	}
	g.mu.Lock()
	d := g.drivers[sess.ID]
	if d == nil {
		d = newSessionDriver(g.r, sess)
		g.drivers[sess.ID] = d
	}
	adopted := g.orphans[sess.ID]
	delete(g.orphans, sess.ID)
	g.mu.Unlock()

	d.updateSession(sess, adopted)
	return d
}

func (g *sessionDriverRegistry) Lookup(sessionID string) (*sessionDriver, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.drivers[sessionID]
	return d, ok
}

func (g *sessionDriverRegistry) PostKnown(ctx context.Context, sessionID string, msg QueuedMessage, wake bool) bool {
	d, ok := g.Lookup(sessionID)
	if !ok {
		return false
	}
	return d.Post(ctx, msg, wake)
}

func (g *sessionDriverRegistry) PostOrBuffer(ctx context.Context, sessionID string, msg QueuedMessage, wake bool) bool {
	if d, ok := g.Lookup(sessionID); ok {
		return d.Post(ctx, msg, wake)
	}
	if !wake {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.orphans[sessionID] = append(g.orphans[sessionID], msg)
	return true
}

func (g *sessionDriverRegistry) Drain(sessionID string) []QueuedMessage {
	d, ok := g.Lookup(sessionID)
	if !ok {
		return nil
	}
	return d.DrainPending()
}

func (g *sessionDriverRegistry) HasPending(sessionID string) bool {
	d, ok := g.Lookup(sessionID)
	return ok && d.HasPending()
}

func (g *sessionDriverRegistry) StopWake(sessionID string) bool {
	d, ok := g.Lookup(sessionID)
	return ok && d.StopWake()
}

func (g *sessionDriverRegistry) StopAll(sessionID string) bool {
	d, ok := g.Lookup(sessionID)
	return ok && d.StopAll()
}

func (g *sessionDriverRegistry) Settled(sessionID string) bool {
	d, ok := g.Lookup(sessionID)
	if !ok {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.orphans[sessionID]) == 0
	}
	return d.Settled()
}

func (g *sessionDriverRegistry) Close() {
	g.mu.Lock()
	drivers := make([]*sessionDriver, 0, len(g.drivers))
	for _, d := range g.drivers {
		drivers = append(drivers, d)
	}
	g.orphans = map[string][]QueuedMessage{}
	g.mu.Unlock()
	for _, d := range drivers {
		d.StopAll()
	}
}

// sessionDriver is the mailbox/driver for one session. It is the only place
// that decides whether detached input is buffered into a live run, wakes an
// idle session, attaches a caller to an in-flight run, or cancels a
// runtime-owned wake run.
type sessionDriver struct {
	r *LocalRuntime

	mu            sync.Mutex
	sess          *session.Session
	running       bool
	wakeRunning   bool
	cancelingWake bool
	stopped       bool
	cancel        context.CancelFunc
	pending       []QueuedMessage
	lastError     string
	onSettled     map[int]func()
	nextHookID    int
	rootActive    bool
	settled       chan struct{}
}

func newSessionDriver(r *LocalRuntime, sess *session.Session) *sessionDriver {
	settled := make(chan struct{})
	close(settled)
	return &sessionDriver{r: r, sess: sess, settled: settled, onSettled: map[int]func(){}}
}

func (d *sessionDriver) updateSession(sess *session.Session, adopted []QueuedMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sess = sess
	if len(adopted) > 0 {
		d.pending = append(d.pending, adopted...)
	}
}

func (d *sessionDriver) Subscribe(buffer int) (seed []Event, events <-chan Event, cancel func()) {
	return d.r.sessionEvents.Subscribe(d.sessionID(), buffer)
}

func (d *sessionDriver) RunOrAttach(ctx context.Context, sess *session.Session) <-chan Event {
	d.updateSession(sess, nil)
	if runCtx, ok := d.tryStart(ctx, false); ok {
		out := make(chan Event, defaultEventChannelCapacity)
		go d.driveToOut(runCtx, false, out)
		return out
	}
	return d.attachThenRun(ctx)
}

func (d *sessionDriver) Post(_ context.Context, msg QueuedMessage, wake bool) bool {
	d.mu.Lock()
	if d.stopped || d.cancelingWake || (!d.running && !wake) {
		d.mu.Unlock()
		return false
	}
	d.pending = append(d.pending, msg)
	if d.running {
		d.mu.Unlock()
		return true
	}
	d.startWakeLocked()
	d.mu.Unlock()
	return true
}

func (d *sessionDriver) DrainPending() []QueuedMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	pending := d.pending
	d.pending = nil
	return pending
}

func (d *sessionDriver) HasPending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending) > 0
}

func (d *sessionDriver) OnSettled(fn func()) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.onSettled == nil {
		d.onSettled = map[int]func(){}
	}
	d.nextHookID++
	id := d.nextHookID
	d.onSettled[id] = fn
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.onSettled, id)
	}
}

func (d *sessionDriver) StopWake() bool {
	d.mu.Lock()
	if !d.running || !d.wakeRunning || d.cancel == nil {
		d.mu.Unlock()
		return false
	}
	cancel := d.cancel
	d.pending = nil
	d.cancelingWake = true
	d.mu.Unlock()
	cancel()
	return true
}

func (d *sessionDriver) StopAll() bool {
	d.mu.Lock()
	cancel := d.cancel
	d.pending = nil
	d.stopped = true
	d.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (d *sessionDriver) Settled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.running && len(d.pending) == 0
}

func (d *sessionDriver) LastError() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastError
}

func (d *sessionDriver) attachThenRun(ctx context.Context) <-chan Event {
	out := make(chan Event, defaultEventChannelCapacity)
	go func() {
		defer close(out)
		emit := func(e Event) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}

		seed, events, cancel := d.Subscribe(defaultEventChannelCapacity)
		defer cancel()
		for _, e := range seed {
			if !emit(e) {
				return
			}
		}

		for {
			d.mu.Lock()
			running := d.running
			settled := d.settled
			d.mu.Unlock()
			if !running {
				if runCtx, ok := d.tryStart(ctx, false); ok {
					d.driveToOutNoClose(runCtx, false, out)
					return
				}
				continue
			}

			select {
			case <-ctx.Done():
				return
			case e, ok := <-events:
				if !ok || !emit(e) {
					return
				}
			case <-settled:
				for {
					select {
					case e, ok := <-events:
						if !ok || !emit(e) {
							return
						}
					default:
						goto drained
					}
				}
			drained:
				if runCtx, ok := d.tryStart(ctx, false); ok {
					d.driveToOutNoClose(runCtx, false, out)
					return
				}
				continue
			}
		}
	}()
	return out
}

func (d *sessionDriver) tryStart(ctx context.Context, wake bool) (context.Context, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running {
		return nil, false
	}
	d.running = true
	d.wakeRunning = wake
	d.rootActive = d.sess != nil && !d.sess.IsSubSession()
	if d.rootActive {
		d.r.activeRootStreams.Add(1)
	}
	d.openSettledLocked()
	runCtx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	return runCtx, true
}

func (d *sessionDriver) driveToOut(ctx context.Context, wake bool, out chan Event) {
	defer close(out)
	d.driveToOutNoClose(ctx, wake, out)
}

func (d *sessionDriver) driveToOutNoClose(ctx context.Context, wake bool, out chan Event) {
	for {
		var runErr string
		cancelled := false
		run := d.r.runStreamRaw(ctx, d.session())
		for event := range run {
			if errEvent, ok := event.(*ErrorEvent); ok && runErr == "" {
				runErr = errEvent.Error
			}
			if cancelled {
				continue
			}
			select {
			case out <- event:
			default:
				select {
				case out <- event:
				case <-ctx.Done():
					cancelled = true
				}
			}
		}
		if !d.finishRun(ctx, wake, runErr) {
			return
		}
	}
}

func (d *sessionDriver) driveWake(ctx context.Context) {
	for {
		var runErr string
		for event := range d.r.runStreamRaw(ctx, d.session()) {
			if errEvent, ok := event.(*ErrorEvent); ok && runErr == "" {
				runErr = errEvent.Error
			}
			// Drained for flow control only; observers and the session event hub
			// publish the wake run to subscribers.
		}
		if !d.finishRun(ctx, true, runErr) {
			return
		}
	}
}

func (d *sessionDriver) finishRun(ctx context.Context, wake bool, runErr string) bool {
	d.mu.Lock()
	if wake && ctx.Err() == nil && len(d.pending) > 0 {
		d.lastError = runErr
		d.mu.Unlock()
		return true
	}
	pending := len(d.pending) > 0
	rootActive := d.rootActive
	var callbacks []func()
	d.lastError = runErr
	if pending && ctx.Err() == nil {
		d.running = false
		d.wakeRunning = false
		d.cancelingWake = false
		d.rootActive = false
		d.cancel = nil
		d.startWakeLocked()
	} else {
		d.running = false
		d.wakeRunning = false
		d.cancelingWake = false
		d.rootActive = false
		d.cancel = nil
		d.closeSettledLocked()
		if !pending {
			callbacks = d.settledCallbacksLocked()
		}
	}
	d.mu.Unlock()
	if rootActive {
		d.r.activeRootStreams.Add(-1)
	}
	for _, fn := range callbacks {
		fn()
	}
	return false
}

// startWakeLocked starts a runtime-owned wake run. Caller holds d.mu and has
// already determined the driver is idle with pending input.
func (d *sessionDriver) startWakeLocked() {
	d.running = true
	d.wakeRunning = true
	d.rootActive = d.sess != nil && !d.sess.IsSubSession()
	if d.rootActive {
		d.r.activeRootStreams.Add(1)
	}
	d.openSettledLocked()
	runCtx, cancel := context.WithCancel(context.WithoutCancel(d.r.ctx()))
	d.cancel = cancel
	go d.driveWake(runCtx)
}

func (d *sessionDriver) settledCallbacksLocked() []func() {
	if len(d.onSettled) == 0 {
		return nil
	}
	callbacks := make([]func(), 0, len(d.onSettled))
	for _, fn := range d.onSettled {
		callbacks = append(callbacks, fn)
	}
	return callbacks
}

func (d *sessionDriver) session() *session.Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sess
}

func (d *sessionDriver) sessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess == nil {
		return ""
	}
	return d.sess.ID
}

func (d *sessionDriver) openSettledLocked() {
	select {
	case <-d.settled:
		d.settled = make(chan struct{})
	default:
	}
}

func (d *sessionDriver) closeSettledLocked() {
	select {
	case <-d.settled:
	default:
		close(d.settled)
	}
}
