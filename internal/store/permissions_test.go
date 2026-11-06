package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenProtectsFilesWithoutChangingExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "alerts.db")
	for _, suffix := range []string{"", ".lock"} {
		if err := os.WriteFile(path+suffix, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	database, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	for _, suffix := range []string{"", ".lock", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", suffix, info.Mode().Perm())
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("existing directory mode changed to %o", info.Mode().Perm())
	}
	for range 2 {
		var version int64
		if err := database.DB().QueryRowContext(t.Context(), `SELECT max(version_id) FROM goose_db_version WHERE is_applied = 1`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != 1 || SchemaVersion != 1 {
			t.Fatalf("database version=%d supported=%d, want 1", version, SchemaVersion)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		database, err = Open(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestProtectDatabaseFilesPreservesRecoveryData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.db")
	const content = "saved recovery data"
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.WriteFile(path+suffix, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := protectDatabaseFiles(path); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil || string(data) != content {
			t.Fatalf("recovery data changed for %s: %q, %v", suffix, data, err)
		}
		info, err := os.Stat(path + suffix)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file %s was not protected: %v, %v", suffix, info, err)
		}
	}
}

func TestOpenReleasesLockAfterFileProtectionFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.db")
	if err := os.Symlink("alerts.db-wal", path+"-wal"); err != nil {
		t.Fatal(err)
	}
	if database, err := Open(t.Context(), path); err == nil {
		_ = database.Close()
		t.Fatal("opened a database with an unprotectable side file")
	}
	if err := os.Remove(path + "-wal"); err != nil {
		t.Fatal(err)
	}
	database, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("failed startup retained the lock: %v", err)
	}
	defer func() { _ = database.Close() }()
}
