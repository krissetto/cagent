package desktop

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetToken(t *testing.T) {
	valid := makeToken(t, time.Now().Add(time.Hour))
	expired := makeToken(t, time.Now().Add(-time.Hour))

	t.Run("valid token returned as-is", func(t *testing.T) {
		backend := &fakeBackend{token: valid}
		installFakeBackend(t, backend)

		assert.Equal(t, valid, GetToken(t.Context()))
		assert.Equal(t, 0, backend.refreshes())
	})

	t.Run("expired token triggers forced refresh", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		backend.onRefresh = func() { backend.setToken(valid) }
		installFakeBackend(t, backend)

		assert.Equal(t, valid, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("stale token returned when refresh does not help", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		installFakeBackend(t, backend)

		assert.Equal(t, expired, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("backoff prevents repeated refresh nudges", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		installFakeBackend(t, backend)

		assert.Equal(t, expired, GetToken(t.Context()))
		assert.Equal(t, expired, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("rate-limited caller reuses last refresh result", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		backend.onRefresh = func() { backend.setToken(valid) }
		installFakeBackend(t, backend)

		assert.Equal(t, valid, GetToken(t.Context()))

		// Desktop regressed to an expired token, but a new nudge is
		// rate-limited: the cached result of the last refresh is reused.
		backend.setToken(makeToken(t, time.Now().Add(-time.Minute)))
		assert.Equal(t, valid, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("concurrent callers share a single refresh", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		backend.onRefresh = func() { backend.setToken(valid) }
		installFakeBackend(t, backend)

		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				assert.Equal(t, valid, GetToken(t.Context()))
			})
		}
		wg.Wait()
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("canceled caller returns promptly with stale token", func(t *testing.T) {
		backend := &fakeBackend{token: expired}
		installFakeBackend(t, backend)
		refreshBudget = time.Second

		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()

		start := time.Now()
		assert.Equal(t, expired, GetToken(ctx))
		assert.Less(t, time.Since(start), 500*time.Millisecond)
	})

	t.Run("non-JWT token returned as-is", func(t *testing.T) {
		backend := &fakeBackend{token: "not-a-jwt"}
		installFakeBackend(t, backend)

		assert.Equal(t, "not-a-jwt", GetToken(t.Context()))
		assert.Equal(t, 0, backend.refreshes())
	})

	t.Run("empty token while signed out returns without refresh", func(t *testing.T) {
		backend := &fakeBackend{}
		installFakeBackend(t, backend)

		assert.Empty(t, GetToken(t.Context()))
		assert.Equal(t, 0, backend.refreshes())
	})

	t.Run("empty token while signed in triggers forced refresh", func(t *testing.T) {
		backend := &fakeBackend{loggedIn: true}
		backend.onRefresh = func() { backend.setToken(valid) }
		installFakeBackend(t, backend)

		assert.Equal(t, valid, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("empty token stays empty when refresh does not help", func(t *testing.T) {
		backend := &fakeBackend{loggedIn: true}
		installFakeBackend(t, backend)

		assert.Empty(t, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("failed token fetch while signed in triggers forced refresh", func(t *testing.T) {
		backend := &fakeBackend{loggedIn: true, failTokenFetch: true}
		backend.onRefresh = func() {
			backend.setToken(valid)
			backend.setFailTokenFetch(false)
		}
		installFakeBackend(t, backend)

		assert.Equal(t, valid, GetToken(t.Context()))
		assert.Equal(t, 1, backend.refreshes())
	})

	t.Run("failed token fetch while signed out returns empty without refresh", func(t *testing.T) {
		backend := &fakeBackend{failTokenFetch: true}
		installFakeBackend(t, backend)

		assert.Empty(t, GetToken(t.Context()))
		assert.Equal(t, 0, backend.refreshes())
	})
}

func TestTokenExpired(t *testing.T) {
	assert.False(t, tokenExpired(makeToken(t, time.Now().Add(time.Minute))))
	assert.False(t, tokenExpired(makeToken(t, time.Now().Add(-10*time.Second))), "within clock-skew leeway")
	assert.True(t, tokenExpired(makeToken(t, time.Now().Add(-time.Minute))))
	assert.False(t, tokenExpired("not-a-jwt"))
}

func makeToken(t *testing.T, exp time.Time) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"exp": exp.Unix()}).SignedString([]byte("secret"))
	require.NoError(t, err)
	return token
}

// fakeBackend emulates Docker Desktop's backend API: GET /registry/token
// serves the current token; GET /registry/is-logged-in reports session state;
// POST /registry/credstore-updated triggers onRefresh (Desktop's async
// AutoLogin).
type fakeBackend struct {
	mu             sync.Mutex
	token          string
	loggedIn       bool
	failTokenFetch bool
	refreshCalls   int
	onRefresh      func()
}

func (b *fakeBackend) setToken(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token = token
}

func (b *fakeBackend) setFailTokenFetch(fail bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failTokenFetch = fail
}

func (b *fakeBackend) refreshes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refreshCalls
}

func (b *fakeBackend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /registry/token", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		token, fail := b.token, b.failTokenFetch
		b.mu.Unlock()
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(token)
	})
	mux.HandleFunc("GET /registry/is-logged-in", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		loggedIn := b.loggedIn
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(loggedIn)
	})
	mux.HandleFunc("POST /registry/credstore-updated", func(http.ResponseWriter, *http.Request) {
		b.mu.Lock()
		b.refreshCalls++
		onRefresh := b.onRefresh
		b.mu.Unlock()
		if onRefresh != nil {
			onRefresh()
		}
	})
	return mux
}

func installFakeBackend(t *testing.T, backend *fakeBackend) {
	t.Helper()

	ln := newMemListener()
	server := &http.Server{Handler: backend.handler()}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = ln.Close()
	})

	oldClient := ClientBackend
	ClientBackend = newRawClient(ln.dial)
	t.Cleanup(func() { ClientBackend = oldClient })

	oldCooldown, oldBackoff, oldBudget, oldInterval := refreshCooldown, refreshFailureBackoff, refreshBudget, refreshPollInterval
	refreshCooldown = time.Hour
	refreshFailureBackoff = time.Hour
	refreshBudget = 150 * time.Millisecond
	refreshPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		refreshCooldown, refreshFailureBackoff, refreshBudget, refreshPollInterval = oldCooldown, oldBackoff, oldBudget, oldInterval
	})

	func() {
		refreshState.Lock()
		defer refreshState.Unlock()
		refreshState.nextAttempt = time.Time{}
		refreshState.inflight = nil
		refreshState.result = ""
	}()

	// Runs first on cleanup (LIFO): a detached refresh goroutine must finish
	// before the fake backend and globals are torn down.
	t.Cleanup(func() { drainInflightRefresh(t) })
}

func drainInflightRefresh(t *testing.T) {
	t.Helper()

	refreshState.Lock()
	inflight := refreshState.inflight
	refreshState.Unlock()

	if inflight == nil {
		return
	}
	select {
	case <-inflight:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight token refresh did not finish")
	}
}

// memListener is an in-memory net.Listener fed by its dial method, so the
// RawClient can talk to a fake backend without a real socket.
type memListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newMemListener() *memListener {
	return &memListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *memListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "mem", Net: "unix"}
}

func (l *memListener) dial(context.Context) (net.Conn, error) {
	clientSide, serverSide := net.Pipe()
	select {
	case l.conns <- serverSide:
		return clientSide, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
