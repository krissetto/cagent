package sqlitestore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/docker/docker-agent/pkg/session"
)

func TestNew_DirectoryNotWritable(t *testing.T) {
	t.Parallel()

	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0o600))

	_, err := New(t.Context(), filepath.Join(blocker, "session.db"))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "cannot create database")
	assert.Contains(t, err.Error(), "not a directory")

	// The error must retain the original filesystem diagnostic even if the
	// recovery path also reports that no backup could be created.
	assert.Contains(t, err.Error(), "blocker")
}

func TestNew_RejectsNewerDatabase(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_newer_db.db")

	// Create a valid store first (applies all known migrations)
	store, err := New(t.Context(), dbPath)
	require.NoError(t, err)
	defer store.Close()

	// Inject a future migration into the database to simulate a newer version
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(),
		"INSERT INTO migrations (id, name, description, applied_at) VALUES (?, ?, ?, ?)",
		9999, "9999_future_migration", "Added by a newer version", "2099-01-01T00:00:00Z")
	require.NoError(t, err)
	db.Close()

	// Opening the store should fail with a clear error about version mismatch
	_, err = New(t.Context(), dbPath)
	require.Error(t, err)
	require.ErrorIs(t, err, session.ErrNewerDatabase)
	assert.Contains(t, err.Error(), "9999")
	assert.Contains(t, err.Error(), "upgrade docker-agent")
}

// TestNew_TransientErrorPreservesDB verifies that a transient failure during
// open (here: a canceled context, e.g. Ctrl-C during startup) does NOT
// trigger the backup-and-reset recovery path, which would silently discard a
// healthy session history.
func TestNew_TransientErrorPreservesDB(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test_transient.db")

	// Create a valid database with one session.
	store, err := New(t.Context(), dbPath)
	require.NoError(t, err)
	require.NoError(t, store.AddSession(t.Context(), &session.Session{ID: "keep-me", CreatedAt: time.Now()}))
	require.NoError(t, store.Close())

	// Opening with an already-canceled context must fail without recovery.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = New(canceled, dbPath)
	require.ErrorIs(t, err, context.Canceled)

	// The database must be untouched: no .bak, data still there.
	_, err = os.Stat(dbPath + ".bak")
	assert.True(t, os.IsNotExist(err), "no backup should be created for transient errors")

	store, err = New(t.Context(), dbPath)
	require.NoError(t, err)
	defer store.Close()
	retrieved, err := store.GetSession(t.Context(), "keep-me")
	require.NoError(t, err)
	assert.Equal(t, "keep-me", retrieved.ID)
}

func TestNew_MigrationFailureRecovery(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_migration_recovery.db")
	backupPath := dbPath + ".bak"

	// Create a corrupted database file that will fail migrations
	err := os.WriteFile(dbPath, []byte("not a valid sqlite database"), 0o644)
	require.NoError(t, err)

	// Opening should trigger recovery: backup the corrupt file and create fresh db
	store, err := New(t.Context(), dbPath)
	require.NoError(t, err)
	defer store.Close()

	// Verify a backup was created
	_, err = os.Stat(backupPath)
	require.NoError(t, err, "backup file should exist")

	// Verify the store works with the fresh database
	sess := &session.Session{
		ID:        "test-session",
		CreatedAt: time.Now(),
	}
	err = store.AddSession(t.Context(), sess)
	require.NoError(t, err)

	retrieved, err := store.GetSession(t.Context(), "test-session")
	require.NoError(t, err)
	assert.Equal(t, "test-session", retrieved.ID)
}

func TestBackupDatabase(t *testing.T) {
	t.Parallel()

	t.Run("backs up existing database file", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "test.db")
		backupPath := dbPath + ".bak"

		// Create a file to backup
		err := os.WriteFile(dbPath, []byte("test content"), 0o644)
		require.NoError(t, err)

		// Also create WAL and SHM files
		err = os.WriteFile(dbPath+"-wal", []byte("wal content"), 0o644)
		require.NoError(t, err)
		err = os.WriteFile(dbPath+"-shm", []byte("shm content"), 0o644)
		require.NoError(t, err)

		// Backup the database
		err = backupDatabase(dbPath)
		require.NoError(t, err)

		// Original should be gone
		_, err = os.Stat(dbPath)
		assert.True(t, os.IsNotExist(err), "original file should be moved")

		// WAL and SHM should also be gone
		_, err = os.Stat(dbPath + "-wal")
		assert.True(t, os.IsNotExist(err), "WAL file should be moved")
		_, err = os.Stat(dbPath + "-shm")
		assert.True(t, os.IsNotExist(err), "SHM file should be moved")

		// Check backup files exist
		_, err = os.Stat(backupPath)
		require.NoError(t, err, "main backup should exist")
		_, err = os.Stat(backupPath + "-wal")
		require.NoError(t, err, "WAL backup should exist")
		_, err = os.Stat(backupPath + "-shm")
		require.NoError(t, err, "SHM backup should exist")
	})

	t.Run("handles nonexistent file gracefully", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "nonexistent.db")

		// Backup should succeed (nothing to backup)
		err := backupDatabase(dbPath)
		require.NoError(t, err)
	})

	t.Run("preserves an existing backup", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "test.db")
		backupPath := dbPath + ".bak"

		// A backup from an earlier recovery, plus a fresh corrupt database.
		require.NoError(t, os.WriteFile(backupPath, []byte("previous backup"), 0o644))
		require.NoError(t, os.WriteFile(dbPath, []byte("new corrupt db"), 0o644))

		require.NoError(t, backupDatabase(dbPath))

		// The prior backup must be untouched...
		prev, err := os.ReadFile(backupPath)
		require.NoError(t, err)
		assert.Equal(t, "previous backup", string(prev))

		// ...and the new file moved aside under a timestamped name.
		matches, err := filepath.Glob(dbPath + ".bak.*")
		require.NoError(t, err)
		require.Len(t, matches, 1)
		moved, err := os.ReadFile(matches[0])
		require.NoError(t, err)
		assert.Equal(t, "new corrupt db", string(moved))
	})
}
