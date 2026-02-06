// Package tabstore provides persistent storage for background agent tab state.
package tabstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/docker/cagent/pkg/paths"
	"github.com/docker/cagent/pkg/sqliteutil"
)

// Tab represents a persisted tab entry.
type Tab struct {
	SessionID    string
	WorkingDir   string
	CreatedAt    time.Time
	LastActiveAt time.Time
}

// Store manages persistent tab state in a SQLite database.
type Store struct {
	db *sql.DB
}

// New creates a new tab store, initializing the database if needed.
func New() (*Store, error) {
	dbPath := filepath.Join(paths.GetDataDir(), "background_agents.db")
	db, err := sqliteutil.OpenDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening tab store: %w", err)
	}

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating tab store: %w", err)
	}

	return store, nil
}

// migrate runs database migrations.
func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS tabs (
			session_id TEXT PRIMARY KEY,
			working_dir TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_active_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS active_tab (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			session_id TEXT NOT NULL
		);
		
		CREATE TABLE IF NOT EXISTS recent_dirs (
			path TEXT PRIMARY KEY,
			used_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	return err
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// AddTab adds a new tab to the store.
func (s *Store) AddTab(ctx context.Context, sessionID, workingDir string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tabs (session_id, working_dir, created_at, last_active_at)
		VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, sessionID, workingDir)
	if err != nil {
		return fmt.Errorf("adding tab: %w", err)
	}

	// Also track the working directory as recently used
	_, err = s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO recent_dirs (path, used_at)
		VALUES (?, CURRENT_TIMESTAMP)
	`, workingDir)
	return err
}

// RemoveTab removes a tab from the store.
func (s *Store) RemoveTab(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tabs WHERE session_id = ?`, sessionID)
	return err
}

// GetTabs returns all persisted tabs.
func (s *Store) GetTabs(ctx context.Context) ([]Tab, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, working_dir, created_at, last_active_at
		FROM tabs
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tabs []Tab
	for rows.Next() {
		var t Tab
		if err := rows.Scan(&t.SessionID, &t.WorkingDir, &t.CreatedAt, &t.LastActiveAt); err != nil {
			return nil, err
		}
		tabs = append(tabs, t)
	}
	return tabs, rows.Err()
}

// SetActiveTab sets the currently active tab.
func (s *Store) SetActiveTab(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO active_tab (id, session_id)
		VALUES (1, ?)
	`, sessionID)
	return err
}

// GetActiveTab returns the currently active tab session ID.
func (s *Store) GetActiveTab(ctx context.Context) (string, error) {
	var sessionID string
	err := s.db.QueryRowContext(ctx, `SELECT session_id FROM active_tab WHERE id = 1`).Scan(&sessionID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sessionID, err
}

// UpdateLastActive updates the last active timestamp for a tab.
func (s *Store) UpdateLastActive(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tabs SET last_active_at = CURRENT_TIMESTAMP
		WHERE session_id = ?
	`, sessionID)
	return err
}

// GetRecentDirs returns the most recently used directories.
func (s *Store) GetRecentDirs(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path FROM recent_dirs
		ORDER BY used_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var dirs []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		dirs = append(dirs, path)
	}
	return dirs, rows.Err()
}

// ClearTabs removes all tabs from the store. Used on clean shutdown.
func (s *Store) ClearTabs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tabs`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM active_tab`)
	return err
}
