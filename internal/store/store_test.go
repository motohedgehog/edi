package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/store"
)

func TestStoreMigratesAndPersistsProfileAndOutbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "edi.db")
	ctx := context.Background()
	database, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	profile, err := database.SaveProfile(ctx, store.Profile{UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true, Timezone: "Europe/London", Lines: []string{"central"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID == 0 || profile.Revision != 1 {
		t.Fatalf("unexpected profile: %+v", profile)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, found, err := database.GetProfile(ctx, 42, store.PersonaTFL)
	if err != nil || !found || len(loaded.Lines) != 1 {
		t.Fatalf("loaded profile: %+v found=%v err=%v", loaded, found, err)
	}
	if _, err := database.SaveProfile(ctx, loaded, loaded.Revision+1); !errors.Is(err, store.ErrRevisionConflict) {
		t.Fatalf("got %v, want revision conflict", err)
	}
	now := time.Now().UTC()
	window := "all-day"
	chunk := store.OutboxChunk{Body: "<b>alert</b>", Format: message.FormatHTML, Operation: "send", NextAttempt: now,
		Candidates: []store.CandidateState{{LineID: "central", Generation: profile.Generation, WindowInstance: window, Fingerprint: "x", ObservedAt: now.UnixMilli()}}}
	if err := database.ReconcileLine(ctx, &chunk, profile.ID, profile.Generation, window, "central"); err != nil {
		t.Fatal(err)
	}
	pending, err := database.PendingChunks(ctx, now.Add(time.Second), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if pending[0].Format != message.FormatHTML || pending[0].Body != chunk.Body {
		t.Fatalf("queued format/body = %v/%q, want HTML/%q", pending[0].Format, pending[0].Body, chunk.Body)
	}
	if err := database.CompleteLine(ctx, pending[0], 11, now); err != nil {
		t.Fatal(err)
	}
	pending, err = database.PendingChunks(ctx, now.Add(time.Second), 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("outbox was not deleted: %+v err=%v", pending, err)
	}
	states, err := database.GetMonitorStates(ctx, profile.ID)
	if err != nil || states["central"].MessageID != 11 {
		t.Fatalf("delivery receipt was not saved: %+v err=%v", states, err)
	}
}

func TestStoreReconcileLineIsIdempotent(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()

	profile, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true,
		Timezone: "Europe/London", Lines: []string{"central"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	nextAttempt := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	window := "window"
	chunk := store.OutboxChunk{Body: "alert", Format: message.FormatHTML, Operation: "send", NextAttempt: nextAttempt,
		Candidates: []store.CandidateState{{LineID: "central", Generation: profile.Generation, WindowInstance: window, Fingerprint: "a"}}}
	for range 2 {
		if err := database.ReconcileLine(ctx, &chunk, profile.ID, profile.Generation, window, "central"); err != nil {
			t.Fatal(err)
		}
	}

	pending, err := database.PendingChunks(ctx, nextAttempt.Add(time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending chunks, want 1: %+v", len(pending), pending)
	}
}

func TestStoreProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edi.db")
	one, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = one.Close() }()
	if _, err := store.Open(context.Background(), path); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("got %v, want lock error", err)
	}
}

func TestStoreQueueAvailabilityPersistsOutageAndRecoveryAtomically(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "edi.db")
	database, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true,
		Timezone: "Europe/London", Lines: []string{"central"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	chunk := availabilityChunk(time.Now().UTC())

	if applied, err := database.QueueAvailability(ctx, profile.ID, profile.Generation, true, []store.OutboxChunk{chunk}); err != nil || !applied {
		t.Fatalf("queue outage = %v, %v; want true, nil", applied, err)
	}
	if applied, err := database.QueueAvailability(ctx, profile.ID, profile.Generation, true, []store.OutboxChunk{chunk}); err != nil || applied {
		t.Fatalf("repeat outage = %v, %v; want false, nil", applied, err)
	}
	if applied, err := database.QueueAvailability(ctx, profile.ID, profile.Generation, false, []store.OutboxChunk{chunk}); err != nil || !applied {
		t.Fatalf("queue recovery = %v, %v; want true, nil", applied, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	loaded, found, err := database.GetProfile(ctx, profile.UserID, store.PersonaTFL)
	if err != nil || !found || loaded.OutageNotified || loaded.OutageGeneration != profile.Generation {
		t.Fatalf("reopened profile = %+v, found=%v, err=%v", loaded, found, err)
	}
	pending, err := database.PendingChunks(ctx, time.Now().Add(time.Minute), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("reopened pending chunks = %+v, err=%v; want latest recovery", pending, err)
	}
	if pending[0].EventType != "outage-recovery" {
		t.Fatalf("availability event = %q", pending[0].EventType)
	}
	for _, chunk := range pending {
		if err := database.DiscardChunk(ctx, chunk); err != nil {
			t.Fatal(err)
		}
	}
	states, err := database.GetMonitorStates(ctx, profile.ID)
	if err != nil || len(states) != 0 {
		t.Fatalf("availability delivery changed line baselines: %+v err=%v", states, err)
	}
	pending, err = database.PendingChunks(ctx, time.Now().Add(time.Minute), 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("delivered availability notices remain queued: %+v err=%v", pending, err)
	}
}

func TestStorePendingChunksRejectsMalformedRows(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	profile, err := database.SaveProfile(ctx, store.Profile{UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = database.DB().ExecContext(ctx, `INSERT INTO outbox_chunks
		(profile_id, revision, generation, event_type, line_ids, body, message_format, chunk_order, chunk_total, candidate_states, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		profile.ID, profile.Revision, profile.Generation, "status", "not-json", "alert", "plain", 1, 1, "[]", now.UnixMilli(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.PendingChunks(ctx, now, 10); err == nil {
		t.Fatal("PendingChunks accepted a malformed line ID row")
	}
}

func TestStoreQueueAvailabilityRejectsDeletedAndStaleProfiles(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	profile, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true,
		Timezone: "Europe/London", Lines: []string{"central"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := database.QueueAvailability(ctx, profile.ID, profile.Generation-1, true, []store.OutboxChunk{availabilityChunk(time.Now())}); err != nil || applied {
		t.Fatalf("stale generation = %v, %v; want false, nil", applied, err)
	}
	if _, err := database.DB().ExecContext(ctx, `DELETE FROM profiles WHERE id = ?`, profile.ID); err != nil {
		t.Fatal(err)
	}
	if applied, err := database.QueueAvailability(ctx, profile.ID, profile.Generation, true, []store.OutboxChunk{availabilityChunk(time.Now())}); err != nil || applied {
		t.Fatalf("deleted profile = %v, %v; want false, nil", applied, err)
	}
}

func TestStoreQueueAvailabilityUsesCurrentRevisionButAcceptsPausedAlerts(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	profile, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true,
		Timezone: "Europe/London", Lines: []string{"central"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	profile.Enabled = false
	profile, err = database.SaveProfile(ctx, profile, profile.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := database.QueueAvailability(ctx, profile.ID, profile.Generation, true, []store.OutboxChunk{availabilityChunk(time.Now())}); err != nil || !applied {
		t.Fatalf("paused alert availability = %v, %v; want true, nil", applied, err)
	}
	pending, err := database.PendingChunks(ctx, time.Now().Add(time.Minute), 10)
	if err != nil || len(pending) != 1 || pending[0].Revision != profile.Revision {
		t.Fatalf("queued chunks = %+v, err=%v; want current revision %d", pending, err, profile.Revision)
	}
}

func TestStoreQueueAvailabilityRollsBackChunksWhenStateUpdateFails(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	profile, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true,
		Timezone: "Europe/London", Lines: []string{"central"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := database.QueueAvailability(ctx, profile.ID, profile.Generation, true, []store.OutboxChunk{availabilityChunk(time.Now())}); err != nil || !applied {
		t.Fatalf("initial outage = %v, %v", applied, err)
	}
	if _, err := database.DB().ExecContext(ctx, `CREATE TRIGGER fail_availability_update
		BEFORE UPDATE OF outage_notified ON profiles BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if applied, err := database.QueueAvailability(ctx, profile.ID, profile.Generation, false, []store.OutboxChunk{availabilityChunk(time.Now())}); err == nil || applied {
		t.Fatalf("failed transition = %v, %v; want false and an error", applied, err)
	}
	loaded, found, err := database.GetProfile(ctx, profile.UserID, store.PersonaTFL)
	if err != nil || !found || !loaded.OutageNotified {
		t.Fatalf("profile after rollback = %+v, found=%v, err=%v", loaded, found, err)
	}
	pending, err := database.PendingChunks(ctx, time.Now().Add(time.Minute), 10)
	if err != nil || len(pending) != 1 || pending[0].EventType != "outage" {
		t.Fatalf("chunks after rollback = %+v, err=%v", pending, err)
	}
}

func TestStoreQueueAvailabilityRejectsEmptyNotice(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	profile, err := database.SaveProfile(ctx, store.Profile{UserID: 42, PersonaID: store.PersonaTFL, AllDay: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := database.QueueAvailability(ctx, profile.ID, profile.Generation, true, nil); err == nil || applied {
		t.Fatalf("empty notice = %v, %v; want false and an error", applied, err)
	}
}

func availabilityChunk(now time.Time) store.OutboxChunk {
	return store.OutboxChunk{Body: "availability", Format: message.FormatHTML, Order: 1, Total: 1, NextAttempt: now}
}

func TestStoreRejectsZeroUserID(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()

	if _, err := database.SaveProfile(ctx, store.Profile{PersonaID: store.PersonaTFL, AllDay: true}, 0); err == nil {
		t.Fatal("zero user ID was accepted")
	}
	if _, found, err := database.GetProfile(ctx, 0, store.PersonaTFL); err != nil || found {
		t.Fatalf("zero user ID wrote a profile: found=%v err=%v", found, err)
	}
}
