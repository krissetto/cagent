package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/subagent"
)

// The SQLite session store doubles as the default [subagent.Store]: subagent
// swarm snapshots live in their own table, keyed by the owning session, and
// are cleaned up automatically when the session is deleted (ON DELETE
// CASCADE). The runtime auto-detects this interface on whatever session store
// it was configured with, so embedders that plug a custom database (e.g.
// Postgres) can implement these two methods on their own store — or supply a
// dedicated implementation via runtime.WithSubagentStore.
var _ subagent.Store = (*SQLiteSessionStore)(nil)

// ensureSubagentTreeTable creates the subagent snapshot table. It runs at
// store open, after migrations, deliberately outside the sessions migration
// catalog: the table is fully owned by the subagent feature, has no schema
// interdependency with session rows beyond the foreign key, and CREATE TABLE
// IF NOT EXISTS is idempotent.
func ensureSubagentTreeTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS subagent_trees (
			session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
			snapshot TEXT NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("creating subagent_trees table: %w", err)
	}
	return nil
}

// SaveTree upserts the swarm snapshot for a session.
func (s *SQLiteSessionStore) SaveTree(ctx context.Context, sessionID string, snapshot subagent.Snapshot) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshaling subagent tree: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO subagent_trees (session_id, snapshot) VALUES (?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET snapshot = excluded.snapshot`,
		sessionID, string(data))
	return err
}

// LoadTree returns the stored swarm snapshot, or nil when the session has none.
func (s *SQLiteSessionStore) LoadTree(ctx context.Context, sessionID string) (*subagent.Snapshot, error) {
	if sessionID == "" {
		return nil, ErrEmptyID
	}
	var data string
	err := s.db.QueryRowContext(ctx,
		`SELECT snapshot FROM subagent_trees WHERE session_id = ?`, sessionID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snapshot := &subagent.Snapshot{}
	if err := json.Unmarshal([]byte(data), snapshot); err != nil {
		return nil, fmt.Errorf("unmarshaling subagent tree: %w", err)
	}
	return snapshot, nil
}
