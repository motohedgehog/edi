// Package store contains the SQLite persistence boundary.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // Register the SQLite database/sql driver.

	"github.com/motohedgehog/edi/internal/logging"
	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/migrations"
	"github.com/pressly/goose/v3"
)

const (
	PersonaTFL             = "tfl"
	SchemaVersion    int64 = 1
	maxOutboxFetch         = 100
	SchedulePeak           = "peak"
	ScheduleAllDay         = "all_day"
	ScheduleCustom         = "custom"
	DefaultPeakStart       = "06:30"
	DefaultPeakEnd         = "09:30"
)

var (
	ErrLocked           = logging.Message("database is already locked by another process")
	ErrRevisionConflict = errors.New("profile revision changed")
)

// Store owns the one SQLite connection and the process lock for its database path.
type Store struct {
	db        *sql.DB
	lockFile  *os.File
	closeOnce sync.Once
}

type Profile struct {
	ID               int64
	UserID           int64
	PersonaID        string
	Revision         int64
	Generation       int64
	Enabled          bool
	AllDay           bool
	ScheduleMode     string
	WindowStart      string
	WindowEnd        string
	Timezone         string
	OutageNotified   bool
	OutageGeneration int64
	Lines            []string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type MonitorState struct {
	SeverityKey    string
	MessageID      int
	SentAt         time.Time
	ProfileID      int64
	LineID         string
	Generation     int64
	WindowInstance string
	Fingerprint    string
	Good           bool
	Unknown        bool
	Description    string
	Reason         string
	ObservedAt     time.Time
}

type CandidateState struct {
	SeverityKey    string `json:"severity_key"`
	LineID         string `json:"line_id"`
	Generation     int64  `json:"generation"`
	WindowInstance string `json:"window_instance"`
	Fingerprint    string `json:"fingerprint"`
	Good           bool   `json:"good"`
	Unknown        bool   `json:"unknown"`
	Description    string `json:"description"`
	Reason         string `json:"reason"`
	ObservedAt     int64  `json:"observed_at"`
}

type OutboxChunk struct {
	Operation   string
	MessageID   int
	Version     int64
	ID          int64
	ProfileID   int64
	Revision    int64
	Generation  int64
	EventType   string
	LineIDs     []string
	Body        string
	Format      message.Format
	Order       int
	Total       int
	Candidates  []CandidateState
	Attempts    int
	NextAttempt time.Time
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type CatalogLine struct {
	ID          string
	DisplayName string
	ModeName    string
	UpdatedAt   time.Time
}

type SeverityRecord struct {
	ModeName      string
	SeverityLevel int
	Description   string
	UpdatedAt     time.Time
}

// Open creates the database directory, acquires the process lock, configures SQLite,
// and applies all embedded migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	absPath, err = canonicalDatabasePath(absPath)
	if err != nil {
		return nil, err
	}
	lockPath := absPath + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database lock: %w", err)
	}
	if err := lockFile.Chmod(0o600); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("protect database lock: %w", err)
	}
	if err := lock(lockFile); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, ErrLocked) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock database: %w", err)
	}
	if err := protectDatabaseFiles(absPath); err != nil {
		_ = unlockAndClose(lockFile)
		return nil, err
	}

	db, err := sql.Open("sqlite", absPath)
	if err != nil {
		_ = unlockAndClose(lockFile)
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	cleanup := func(openErr error) (*Store, error) {
		_ = db.Close()
		_ = unlockAndClose(lockFile)
		return nil, openErr
	}
	if err := configureSQLite(ctx, db); err != nil {
		return cleanup(fmt.Errorf("configure sqlite: %w", err))
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.FS)
	if err != nil {
		return cleanup(fmt.Errorf("create migration provider: %w", err))
	}
	currentVersion, err := provider.GetDBVersion(ctx)
	if err != nil {
		return cleanup(fmt.Errorf("read schema version: %w", err))
	}
	if currentVersion > SchemaVersion {
		return cleanup(logging.Message(fmt.Sprintf("database schema version %d is newer than supported version %d", currentVersion, SchemaVersion)))
	}
	if _, err := provider.Up(ctx); err != nil {
		return cleanup(fmt.Errorf("apply migrations: %w", err))
	}
	return &Store{db: db, lockFile: lockFile}, nil
}

func canonicalDatabasePath(path string) (string, error) {
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("resolve database directory: %w", err)
	}
	path = filepath.Join(dir, filepath.Base(path))
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return path, nil
	} else if err != nil {
		return "", fmt.Errorf("inspect database path: %w", err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve database links: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect database file: %w", err)
	}
	// SQLite recovery files follow the pathname, so hardlink aliases are unsafe.
	if info.Sys().(*syscall.Stat_t).Nlink > 1 {
		return "", logging.Message("database must not have multiple hard links")
	}
	return path, nil
}

func protectDatabaseFiles(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create database file: %w", err)
	}
	if err := errors.Join(file.Chmod(0o600), file.Close()); err != nil {
		return fmt.Errorf("protect database file: %w", err)
	}
	// SQLite inherits the database mode for new side files; protect existing ones too.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("protect database side file: %w", err)
		}
	}
	return nil
}

func configureSQLite(ctx context.Context, db *sql.DB) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("%s: %w", statement, err)
		}
	}
	var foreignKeys, busyTimeout int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("verify foreign keys: %w", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		return fmt.Errorf("verify busy timeout: %w", err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 {
		return logging.Message("sqlite pragmas are not active")
	}
	return nil
}

// Close releases the database and the process lock. It is safe to call more than once.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		if closeErr := s.db.Close(); closeErr != nil {
			err = closeErr
		}
		if unlockErr := unlockAndClose(s.lockFile); unlockErr != nil && err == nil {
			err = unlockErr
		}
	})
	return err
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) GetProfile(ctx context.Context, userID int64, personaID string) (Profile, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, persona_id, revision, generation, enabled, all_day, schedule_mode,
		       COALESCE(window_start, ''), COALESCE(window_end, ''), timezone,
		       outage_notified, outage_generation, created_at, updated_at
		FROM profiles WHERE user_id = ? AND persona_id = ?`, userID, personaID)
	profile, err := scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, fmt.Errorf("get profile: %w", err)
	}
	lines, err := s.profileLines(ctx, profile.ID)
	if err != nil {
		return Profile{}, false, err
	}
	profile.Lines = lines
	return profile, true, nil
}

func (s *Store) GetProfileByID(ctx context.Context, profileID int64) (Profile, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, persona_id, revision, generation, enabled, all_day, schedule_mode,
		       COALESCE(window_start, ''), COALESCE(window_end, ''), timezone,
		       outage_notified, outage_generation, created_at, updated_at
		FROM profiles WHERE id = ?`, profileID)
	profile, err := scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, fmt.Errorf("get profile by ID: %w", err)
	}
	lines, err := s.profileLines(ctx, profile.ID)
	if err != nil {
		return Profile{}, false, err
	}
	profile.Lines = lines
	return profile, true, nil
}

func (s *Store) ListProfiles(ctx context.Context, personaID string) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, persona_id, revision, generation, enabled, all_day, schedule_mode,
		       COALESCE(window_start, ''), COALESCE(window_end, ''), timezone,
		       outage_notified, outage_generation, created_at, updated_at
		FROM profiles WHERE persona_id = ? ORDER BY user_id`, personaID)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var profiles []Profile
	for rows.Next() {
		profile, scanErr := scanProfile(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan profile: %w", scanErr)
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate profiles: %w", err)
	}
	for index := range profiles {
		lines, lineErr := s.profileLines(ctx, profiles[index].ID)
		if lineErr != nil {
			return nil, lineErr
		}
		profiles[index].Lines = lines
	}
	return profiles, nil
}

func scanProfile(scanner interface{ Scan(...any) error }) (Profile, error) {
	var profile Profile
	var enabled, allDay, outageNotified int
	var windowStart, windowEnd, scheduleMode string
	var createdAt, updatedAt int64
	if err := scanner.Scan(&profile.ID, &profile.UserID, &profile.PersonaID, &profile.Revision,
		&profile.Generation, &enabled, &allDay, &scheduleMode, &windowStart, &windowEnd, &profile.Timezone,
		&outageNotified, &profile.OutageGeneration, &createdAt, &updatedAt); err != nil {
		return Profile{}, err
	}
	profile.Enabled = enabled == 1
	profile.AllDay = allDay == 1
	profile.ScheduleMode = normalizeScheduleMode(scheduleMode, profile.AllDay, windowStart, windowEnd)
	profile.AllDay = profile.ScheduleMode == ScheduleAllDay
	profile.WindowStart = windowStart
	profile.WindowEnd = windowEnd
	profile.OutageNotified = outageNotified == 1
	profile.CreatedAt = time.UnixMilli(createdAt).UTC()
	profile.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return profile, nil
}

func (s *Store) profileLines(ctx context.Context, profileID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT line_id FROM profile_lines WHERE profile_id = ? ORDER BY line_id`, profileID)
	if err != nil {
		return nil, fmt.Errorf("get profile lines: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("scan profile line: %w", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate profile lines: %w", err)
	}
	return lines, nil
}

// SaveProfile atomically persists a profile and its line selection. expectedRevision is
// zero for a new profile; a nonzero value protects an existing draft from stale saves.
func (s *Store) SaveProfile(ctx context.Context, profile Profile, expectedRevision int64) (Profile, error) {
	if profile.UserID <= 0 {
		return Profile{}, errors.New("profile user ID must be positive")
	}
	if profile.PersonaID == "" {
		profile.PersonaID = PersonaTFL
	}
	if profile.Timezone == "" {
		profile.Timezone = "Europe/London"
	}
	profile.ScheduleMode = normalizeScheduleMode(profile.ScheduleMode, profile.AllDay, profile.WindowStart, profile.WindowEnd)
	profile.AllDay = profile.ScheduleMode == ScheduleAllDay
	if profile.WindowStart == "" {
		profile.WindowStart = DefaultPeakStart
	}
	if profile.WindowEnd == "" {
		profile.WindowEnd = DefaultPeakEnd
	}
	now := time.Now().UTC().UnixMilli()
	var saved Profile
	err := s.write(ctx, func(conn *sql.Conn) error {
		var id, revision, generation int64
		var oldEnabled, oldAllDay int
		var oldStart, oldEnd, oldTimezone, oldScheduleMode string
		var oldCreatedAt, oldUpdatedAt int64
		var oldOutageNotified int
		var oldOutageGeneration int64
		selectErr := conn.QueryRowContext(ctx, `SELECT id, revision, generation, enabled, all_day, schedule_mode,
			COALESCE(window_start, ''), COALESCE(window_end, ''), timezone,
			outage_notified, outage_generation, created_at, updated_at
			FROM profiles WHERE user_id = ? AND persona_id = ?`, profile.UserID, profile.PersonaID).
			Scan(&id, &revision, &generation, &oldEnabled, &oldAllDay, &oldScheduleMode, &oldStart, &oldEnd,
				&oldTimezone, &oldOutageNotified, &oldOutageGeneration, &oldCreatedAt, &oldUpdatedAt)
		if errors.Is(selectErr, sql.ErrNoRows) {
			if expectedRevision != 0 {
				return ErrRevisionConflict
			}
			generation = 1
			revision = 1
			if profile.Generation > 0 {
				generation = profile.Generation
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO profiles
				(user_id, persona_id, revision, generation, enabled, all_day, schedule_mode, window_start, window_end, timezone,
				 outage_notified, outage_generation, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, 0, 0, ?, ?)`,
				profile.UserID, profile.PersonaID, revision, generation, boolInt(profile.Enabled), boolInt(profile.AllDay), profile.ScheduleMode,
				profile.WindowStart, profile.WindowEnd, profile.Timezone, now, now); err != nil {
				return fmt.Errorf("insert profile: %w", err)
			}
			if err := conn.QueryRowContext(ctx, `SELECT id FROM profiles WHERE user_id = ? AND persona_id = ?`, profile.UserID, profile.PersonaID).Scan(&id); err != nil {
				return fmt.Errorf("read inserted profile: %w", err)
			}
		} else if selectErr != nil {
			return fmt.Errorf("read profile for update: %w", selectErr)
		} else {
			if expectedRevision == 0 || expectedRevision != revision {
				return ErrRevisionConflict
			}
			oldScheduleMode = normalizeScheduleMode(oldScheduleMode, oldAllDay == 1, oldStart, oldEnd)
			oldLines, linesErr := profileLinesConn(ctx, conn, id)
			if linesErr != nil {
				return linesErr
			}
			configChanged := oldScheduleMode != profile.ScheduleMode || oldStart != profile.WindowStart || oldEnd != profile.WindowEnd || oldTimezone != profile.Timezone ||
				!sameStringsMustSorted(profile.Lines, oldLines)
			if !configChanged && oldEnabled == boolInt(profile.Enabled) {
				profile.ID = id
				profile.Revision = revision
				profile.Generation = generation
				profile.CreatedAt = time.UnixMilli(oldCreatedAt).UTC()
				profile.UpdatedAt = time.UnixMilli(oldUpdatedAt).UTC()
				profile.OutageNotified = oldOutageNotified == 1
				profile.OutageGeneration = oldOutageGeneration
				saved = profile
				return nil
			}
			if configChanged {
				generation++
				if _, err := conn.ExecContext(ctx, `DELETE FROM monitor_states WHERE profile_id = ? OR profile_id IN
					(SELECT id FROM profiles WHERE id = ?)`, id, id); err != nil {
					return fmt.Errorf("reset monitor states: %w", err)
				}
				if _, err := conn.ExecContext(ctx, `DELETE FROM outbox_chunks WHERE profile_id = ? AND event_type = 'status'`, id); err != nil {
					return fmt.Errorf("cancel pending alerts: %w", err)
				}
			}
			outageGeneration := oldOutageGeneration
			if configChanged && oldOutageNotified == 1 {
				outageGeneration = generation
			}
			revision++
			if _, err := conn.ExecContext(ctx, `UPDATE profiles SET revision = ?, generation = ?, enabled = ?, all_day = ?, schedule_mode = ?,
				window_start = NULLIF(?, ''), window_end = NULLIF(?, ''), timezone = ?, outage_generation = ?, updated_at = ? WHERE id = ?`,
				revision, generation, boolInt(profile.Enabled), boolInt(profile.AllDay), profile.ScheduleMode,
				profile.WindowStart, profile.WindowEnd, profile.Timezone, outageGeneration, now, id); err != nil {
				return fmt.Errorf("update profile: %w", err)
			}
			profile.OutageNotified = oldOutageNotified == 1
			profile.OutageGeneration = outageGeneration
			profile.CreatedAt = time.UnixMilli(oldCreatedAt).UTC()
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM profile_lines WHERE profile_id = ?`, id); err != nil {
			return fmt.Errorf("replace profile lines: %w", err)
		}
		for _, line := range uniqueSorted(profile.Lines) {
			if strings.TrimSpace(line) == "" {
				return errors.New("profile line ID cannot be empty")
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO profile_lines (profile_id, line_id) VALUES (?, ?)`, id, line); err != nil {
				return fmt.Errorf("insert profile line: %w", err)
			}
		}
		profile.ID = id
		profile.Revision = revision
		profile.Generation = generation
		if profile.CreatedAt.IsZero() {
			profile.CreatedAt = time.UnixMilli(now).UTC()
		}
		profile.UpdatedAt = time.UnixMilli(now).UTC()
		saved = profile
		return nil
	})
	return saved, err
}

func profileLinesConn(ctx context.Context, conn *sql.Conn, profileID int64) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT line_id FROM profile_lines WHERE profile_id = ?`, profileID)
	if err != nil {
		return nil, fmt.Errorf("read profile lines for update: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("scan profile lines for update: %w", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate profile lines for update: %w", err)
	}
	return lines, nil
}

func sameStringsMustSorted(a, b []string) bool {
	return strings.Join(uniqueSorted(a), "\x00") == strings.Join(uniqueSorted(b), "\x00")
}

func uniqueSorted(values []string) []string {
	copyValues := append([]string(nil), values...)
	sort.Strings(copyValues)
	return slices.Compact(copyValues)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func normalizeScheduleMode(mode string, allDay bool, start, end string) string {
	switch mode {
	case SchedulePeak, ScheduleAllDay, ScheduleCustom:
		return mode
	}
	if allDay {
		return ScheduleAllDay
	}
	if start != "" || end != "" {
		return ScheduleCustom
	}
	return SchedulePeak
}

// SetProfileEnabled changes only the saved alert status. It does not advance
// the monitoring generation, so a status toggle cannot create a new baseline.
func (s *Store) SetProfileEnabled(ctx context.Context, userID int64, personaID string, enabled bool) (Profile, bool, error) {
	profile, found, err := s.GetProfile(ctx, userID, personaID)
	if err != nil || !found || profile.Enabled == enabled {
		return profile, found, err
	}
	profile.Enabled = enabled
	saved, err := s.SaveProfile(ctx, profile, profile.Revision)
	if err != nil {
		return Profile{}, false, fmt.Errorf("set profile alert status: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM outbox_chunks WHERE profile_id = ? AND event_type = 'status'`, saved.ID); err != nil {
		return Profile{}, false, fmt.Errorf("cancel pending line alerts: %w", err)
	}
	return saved, true, nil
}

func (s *Store) DeleteProfile(ctx context.Context, userID int64, personaID string) error {
	return s.write(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM profiles WHERE user_id = ? AND persona_id = ?`, userID, personaID); err != nil {
			return fmt.Errorf("delete profile: %w", err)
		}
		return nil
	})
}

func (s *Store) GetMonitorStates(ctx context.Context, profileID int64) (map[string]MonitorState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT profile_id, line_id, generation, window_instance, fingerprint, good, unknown,
		description, reason, observed_at, severity_key, message_id, sent_at FROM monitor_states WHERE profile_id = ?`, profileID)
	if err != nil {
		return nil, fmt.Errorf("get monitor states: %w", err)
	}
	defer func() { _ = rows.Close() }()
	states := make(map[string]MonitorState)
	for rows.Next() {
		var state MonitorState
		var good, unknown int
		var observed int64
		var sent sql.NullInt64
		if err := rows.Scan(&state.ProfileID, &state.LineID, &state.Generation, &state.WindowInstance, &state.Fingerprint, &good, &unknown,
			&state.Description, &state.Reason, &observed, &state.SeverityKey, &state.MessageID, &sent); err != nil {
			return nil, fmt.Errorf("scan monitor state: %w", err)
		}
		if sent.Valid {
			state.SentAt = time.UnixMilli(sent.Int64).UTC()
		}
		state.Good = good == 1
		state.Unknown = unknown == 1
		state.ObservedAt = time.UnixMilli(observed).UTC()
		states[state.LineID] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate monitor states: %w", err)
	}
	return states, nil
}

func (s *Store) SaveMonitorStates(ctx context.Context, states []MonitorState) error {
	if len(states) == 0 {
		return nil
	}
	return s.write(ctx, func(conn *sql.Conn) error {
		for _, state := range states {
			if _, err := conn.ExecContext(ctx, `INSERT INTO monitor_states
				(profile_id, line_id, generation, window_instance, fingerprint, good, unknown, description, reason, observed_at, severity_key, message_id, sent_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(profile_id, line_id) DO UPDATE SET generation=excluded.generation,
				window_instance=excluded.window_instance, fingerprint=excluded.fingerprint, good=excluded.good, unknown=excluded.unknown,
				description=excluded.description, reason=excluded.reason, observed_at=excluded.observed_at, severity_key=excluded.severity_key, message_id=excluded.message_id, sent_at=excluded.sent_at`,
				state.ProfileID, state.LineID, state.Generation, state.WindowInstance, state.Fingerprint, boolInt(state.Good), boolInt(state.Unknown),
				state.Description, state.Reason, state.ObservedAt.UTC().UnixMilli(), state.SeverityKey, state.MessageID, optionalTime(state.SentAt)); err != nil {
				return fmt.Errorf("save monitor state: %w", err)
			}
		}
		return nil
	})
}

// QueueAvailability atomically persists an availability notice and its profile state.
func (s *Store) QueueAvailability(ctx context.Context, profileID, generation int64, unavailable bool, chunks []OutboxChunk) (bool, error) {
	if len(chunks) == 0 {
		return false, errors.New("queue availability notice: chunks are required")
	}
	eventType := "outage-recovery"
	if unavailable {
		eventType = "outage"
	}
	var applied bool
	err := s.write(ctx, func(conn *sql.Conn) error {
		var revision, currentGeneration int64
		var outageNotified int
		err := conn.QueryRowContext(ctx, `SELECT revision, generation, outage_notified
			FROM profiles WHERE id = ?`, profileID).Scan(&revision, &currentGeneration, &outageNotified)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load profile for availability transition: %w", err)
		}
		if currentGeneration != generation {
			return nil
		}
		if (outageNotified == 1) == unavailable {
			return nil
		}
		var attempts int
		var nextAttempt int64
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempts),0), COALESCE(MAX(next_attempt_at),0)
			FROM outbox_chunks WHERE profile_id=? AND event_type IN ('outage','outage-recovery')`, profileID).Scan(&attempts, &nextAttempt); err != nil {
			return fmt.Errorf("load availability retry state: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM outbox_chunks WHERE profile_id=? AND event_type IN ('outage','outage-recovery')`, profileID); err != nil {
			return fmt.Errorf("replace pending availability notice: %w", err)
		}
		pending := append([]OutboxChunk(nil), chunks...)
		for i := range pending {
			pending[i].Attempts = max(pending[i].Attempts, attempts)
			if pending[i].NextAttempt.UnixMilli() < nextAttempt {
				pending[i].NextAttempt = time.UnixMilli(nextAttempt).UTC()
			}
		}
		if err := insertChunks(ctx, conn, profileID, revision, generation, eventType, pending); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE profiles SET outage_notified = ?, outage_generation = ?, updated_at = ?
			WHERE id = ? AND generation = ?`, boolInt(unavailable), generation, time.Now().UTC().UnixMilli(), profileID, generation); err != nil {
			return fmt.Errorf("update availability state: %w", err)
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

func insertChunks(ctx context.Context, conn *sql.Conn, profileID, revision, generation int64, eventType string, chunks []OutboxChunk) error {
	for _, chunk := range chunks {
		if chunk.Format != message.FormatPlainText && chunk.Format != message.FormatHTML {
			return fmt.Errorf("enqueue outbox chunk: invalid message format %d", chunk.Format)
		}
		candidateJSON, err := json.Marshal(chunk.Candidates)
		if err != nil {
			return fmt.Errorf("encode candidate states: %w", err)
		}
		lineIDs, err := json.Marshal(uniqueSorted(chunk.LineIDs))
		if err != nil {
			return fmt.Errorf("encode line IDs: %w", err)
		}
		var expires any
		if !chunk.ExpiresAt.IsZero() {
			expires = chunk.ExpiresAt.UTC().UnixMilli()
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO outbox_chunks
				(profile_id, revision, generation, event_type, line_ids, body, message_format, chunk_order, chunk_total,
				 candidate_states, attempts, next_attempt_at, created_at, expires_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(profile_id, generation, event_type, line_ids, chunk_order) DO NOTHING`, profileID, revision, generation, eventType,
			string(lineIDs), chunk.Body, chunk.Format.String(), chunk.Order, chunk.Total, string(candidateJSON), chunk.Attempts,
			chunk.NextAttempt.UTC().UnixMilli(), time.Now().UTC().UnixMilli(), expires); err != nil {
			return fmt.Errorf("enqueue outbox chunk: %w", err)
		}
	}
	return nil
}

func (s *Store) PendingChunks(ctx context.Context, now time.Time, limit int) ([]OutboxChunk, error) {
	if limit <= 0 || limit > maxOutboxFetch {
		limit = maxOutboxFetch
	}
	if err := s.write(ctx, func(conn *sql.Conn) error {
		if err := normalizeAvailability(ctx, conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM outbox_chunks
			WHERE event_type = 'status' AND expires_at IS NOT NULL AND expires_at <= ?`, now.UTC().UnixMilli()); err != nil {
			return fmt.Errorf("discard expired line alerts: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, profile_id, revision, generation, event_type, line_ids, body, message_format,
		chunk_order, chunk_total, candidate_states, attempts, next_attempt_at, created_at, expires_at, operation, message_id, version
		FROM outbox_chunks WHERE next_attempt_at <= ? ORDER BY id LIMIT ?`, now.UTC().UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("list pending outbox chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var chunks []OutboxChunk
	for rows.Next() {
		var chunk OutboxChunk
		var lineIDs, candidateJSON, format string
		var nextAttempt, createdAt int64
		var expiresAt sql.NullInt64
		if err := rows.Scan(&chunk.ID, &chunk.ProfileID, &chunk.Revision, &chunk.Generation, &chunk.EventType,
			&lineIDs, &chunk.Body, &format, &chunk.Order, &chunk.Total, &candidateJSON, &chunk.Attempts, &nextAttempt, &createdAt, &expiresAt, &chunk.Operation, &chunk.MessageID, &chunk.Version); err != nil {
			return nil, fmt.Errorf("scan pending outbox chunk: %w", err)
		}
		chunk.Format, err = message.ParseFormat(format)
		if err != nil {
			return nil, fmt.Errorf("decode pending message format: %w", err)
		}
		if err := json.Unmarshal([]byte(lineIDs), &chunk.LineIDs); err != nil {
			return nil, fmt.Errorf("decode pending line IDs: %w", err)
		}
		if err := json.Unmarshal([]byte(candidateJSON), &chunk.Candidates); err != nil {
			return nil, fmt.Errorf("decode pending candidate states: %w", err)
		}
		chunk.NextAttempt = time.UnixMilli(nextAttempt).UTC()
		chunk.CreatedAt = time.UnixMilli(createdAt).UTC()
		if expiresAt.Valid {
			chunk.ExpiresAt = time.UnixMilli(expiresAt.Int64).UTC()
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending outbox chunks: %w", err)
	}
	return chunks, nil
}

func normalizeAvailability(ctx context.Context, conn *sql.Conn) error {
	// Old queues may contain both transitions; keep their longest retry before pruning.
	if _, err := conn.ExecContext(ctx, `UPDATE outbox_chunks AS pending SET
		(attempts,next_attempt_at) = (SELECT MAX(attempts),MAX(next_attempt_at) FROM outbox_chunks
			WHERE profile_id=pending.profile_id AND event_type IN ('outage','outage-recovery'))
		WHERE event_type IN ('outage','outage-recovery')`); err != nil {
		return fmt.Errorf("preserve availability retry state: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM outbox_chunks
		WHERE event_type IN ('outage','outage-recovery') AND id NOT IN (
			SELECT MAX(pending.id) FROM outbox_chunks AS pending JOIN profiles ON profiles.id=pending.profile_id
			WHERE pending.event_type=CASE profiles.outage_notified WHEN 1 THEN 'outage' ELSE 'outage-recovery' END
			GROUP BY pending.profile_id)`); err != nil {
		return fmt.Errorf("normalize pending availability notices: %w", err)
	}
	return nil
}

// DiscardChunk removes a queued notice without committing candidate monitor state.
func (s *Store) DiscardChunk(ctx context.Context, chunk OutboxChunk) error {
	return s.write(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM outbox_chunks WHERE id = ?`, chunk.ID); err != nil {
			return fmt.Errorf("discard outbox chunk: %w", err)
		}
		return nil
	})
}

func (s *Store) RescheduleChunk(ctx context.Context, id int64, attempts int, next time.Time) error {
	return s.write(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `UPDATE outbox_chunks SET attempts = ?, next_attempt_at = ? WHERE id = ?`, attempts, next.UTC().UnixMilli(), id); err != nil {
			return fmt.Errorf("reschedule outbox chunk: %w", err)
		}
		return nil
	})
}

func (s *Store) SaveCatalog(ctx context.Context, lines []CatalogLine) error {
	return s.write(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM catalog_lines`); err != nil {
			return fmt.Errorf("clear catalog: %w", err)
		}
		for _, line := range lines {
			if _, err := conn.ExecContext(ctx, `INSERT INTO catalog_lines (line_id, display_name, mode_name, updated_at) VALUES (?, ?, ?, ?)`,
				line.ID, line.DisplayName, line.ModeName, line.UpdatedAt.UTC().UnixMilli()); err != nil {
				return fmt.Errorf("save catalog line: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) LoadCatalog(ctx context.Context) ([]CatalogLine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT line_id, display_name, mode_name, updated_at FROM catalog_lines ORDER BY display_name, line_id`)
	if err != nil {
		return nil, fmt.Errorf("load catalog: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var lines []CatalogLine
	for rows.Next() {
		var line CatalogLine
		var updated int64
		if err := rows.Scan(&line.ID, &line.DisplayName, &line.ModeName, &updated); err != nil {
			return nil, fmt.Errorf("scan catalog line: %w", err)
		}
		line.UpdatedAt = time.UnixMilli(updated).UTC()
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate catalog: %w", err)
	}
	return lines, nil
}

func (s *Store) SaveSeverities(ctx context.Context, records []SeverityRecord) error {
	return s.write(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM severity_metadata`); err != nil {
			return fmt.Errorf("clear severity metadata: %w", err)
		}
		for _, record := range records {
			if _, err := conn.ExecContext(ctx, `INSERT INTO severity_metadata (mode_name, severity_level, description, updated_at) VALUES (?, ?, ?, ?)`,
				record.ModeName, record.SeverityLevel, record.Description, record.UpdatedAt.UTC().UnixMilli()); err != nil {
				return fmt.Errorf("save severity metadata: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) LoadSeverities(ctx context.Context) ([]SeverityRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT mode_name, severity_level, description, updated_at FROM severity_metadata ORDER BY mode_name, severity_level`)
	if err != nil {
		return nil, fmt.Errorf("load severity metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []SeverityRecord
	for rows.Next() {
		var record SeverityRecord
		var updated int64
		if err := rows.Scan(&record.ModeName, &record.SeverityLevel, &record.Description, &updated); err != nil {
			return nil, fmt.Errorf("scan severity metadata: %w", err)
		}
		record.UpdatedAt = time.UnixMilli(updated).UTC()
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate severity metadata: %w", err)
	}
	return records, nil
}

func (s *Store) write(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire sqlite connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate transaction: %w", err)
	}
	if err := fn(conn); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
