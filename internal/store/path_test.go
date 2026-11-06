package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenUsesOneLockForDatabaseAliases(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"direct then alias", "alerts.db", "alias.db"},
		{"alias then direct", "alias.db", "alerts.db"},
		{"relative chain then alias", "chain.db", "alias.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "alerts.db")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			for alias, target := range map[string]string{"alias.db": "alerts.db", "chain.db": "alias.db"} {
				if err := os.Symlink(target, filepath.Join(dir, alias)); err != nil {
					t.Fatal(err)
				}
			}
			database, err := Open(t.Context(), filepath.Join(dir, tc.first))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			if second, err := Open(t.Context(), filepath.Join(dir, tc.second)); !errors.Is(err, ErrLocked) {
				if second != nil {
					_ = second.Close()
				}
				t.Fatalf("second open = %v, want ErrLocked", err)
			}
		})
	}
}

func TestOpenCreatesDatabaseUnderSymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	database, err := Open(t.Context(), filepath.Join(dir, "alias", "new", "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if second, err := Open(t.Context(), filepath.Join(realDir, "new", "alerts.db")); !errors.Is(err, ErrLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("direct open = %v, want ErrLocked", err)
	}
}

func TestOpenRejectsUnusableDatabaseLinks(t *testing.T) {
	for _, kind := range []string{"dangling", "loop", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "alias.db")
			target := filepath.Join(dir, "alerts.db")
			var err error
			switch kind {
			case "dangling":
				err = os.Symlink(target, path)
			case "loop":
				err = os.Symlink("alias.db", path)
			case "hardlink":
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				err = os.Link(target, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{path}
			if kind == "hardlink" {
				paths = append(paths, target)
			}
			for _, name := range paths {
				if database, err := Open(t.Context(), name); err == nil {
					_ = database.Close()
					t.Errorf("opened %s database", kind)
				}
			}
			if kind == "dangling" {
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("dangling link created its target: %v", err)
				}
			}
		})
	}
}

func TestOpenProtectsCanonicalRecoveryFilesAndReleasesLockAfterFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.db")
	alias := filepath.Join(dir, "alias.db")
	if err := os.Symlink("alerts.db", alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	const recovery = "saved recovery data"
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(path+suffix, []byte(recovery), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("alerts.db-journal", path+"-journal"); err != nil {
		t.Fatal(err)
	}
	if database, err := Open(t.Context(), alias); err == nil {
		_ = database.Close()
		t.Fatal("opened with unprotectable recovery file")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		body, err := os.ReadFile(path + suffix)
		if err != nil || string(body) != recovery {
			t.Fatalf("recovery contents changed: %q, %v", body, err)
		}
		info, err := os.Stat(path + suffix)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("canonical recovery file not protected: %v, %v", info, err)
		}
	}
	before, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil {
			t.Fatal(err)
		}
	}
	database, err := Open(t.Context(), alias)
	if err != nil {
		t.Fatalf("failed startup retained canonical lock: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	after, err := os.Stat(path + ".lock")
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("canonical lock was replaced: %v", err)
	}
}
