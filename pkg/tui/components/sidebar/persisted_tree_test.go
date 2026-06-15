package sidebar

import (
	"strings"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestPersistedSessionTreeShowsReloadedChildAndGrandchildClosed(t *testing.T) {
	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithTitle("Root"))
	child := session.NewRuntimeManagedSubSession(root, session.WithID("child12345"), session.WithTitle("Persisted Child"), session.WithAgentName("greppy"))
	child.CreatedAt = time.Now().Add(-2 * time.Minute)
	grandchild := session.NewRuntimeManagedSubSession(child, session.WithID("grand12345"), session.WithTitle("Persisted Grandchild"), session.WithAgentName("reviewer"))
	grandchild.CreatedAt = time.Now().Add(-1 * time.Minute)
	requireNoError(t, store.AddSession(ctx, root))
	requireNoError(t, store.AddSubSession(ctx, "root", child))
	requireNoError(t, store.AddSubSession(ctx, "child12345", grandchild))

	m := New(&service.SessionState{}).(*model)
	m.SetPersistedSessionTree(root, store)
	plain := strings.Join(stripANSILines(strings.Split(m.subagentsSection(100), "\n")), "\n")

	for _, want := range []string{"Subagents", "greppy", "child", "reviewer", "grand", "finalized"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("expected %q in persisted subagent tree: %q", want, plain)
		}
	}
	for _, notWant := range []string{"Persisted Child", "Persisted Grandchild"} {
		if strings.Contains(plain, notWant) {
			t.Fatalf("persisted subagent rows should not render generated titles: %q", plain)
		}
	}
	if strings.Contains(plain, "working") || strings.Contains(plain, "running") {
		t.Fatalf("persisted non-live children must not render as running: %q", plain)
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
