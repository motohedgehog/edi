package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/store"
)

func openAvailabilityStore(t *testing.T, path string) (*store.Store, store.Profile) {
	t.Helper()
	database, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	profile, err := database.SaveProfile(t.Context(), store.Profile{UserID: 42, AllDay: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return database, profile
}

func TestAvailabilityReplacesPendingStateWithoutResettingRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.db")
	database, profile := openAvailabilityStore(t, path)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	chunk := availabilityChunk(now)
	chunk.Body = "first outage"
	if _, err := database.QueueAvailability(t.Context(), profile.ID, profile.Generation, true, []store.OutboxChunk{chunk}); err != nil {
		t.Fatal(err)
	}
	pending, err := database.PendingChunks(t.Context(), now, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("initial pending=%v err=%v", pending, err)
	}
	old := pending[0]
	retry := now.Add(time.Hour)
	if err := database.RescheduleChunk(t.Context(), old.ID, 4, retry); err != nil {
		t.Fatal(err)
	}
	for i, unavailable := range []bool{false, true} {
		chunk.Body = []string{"recovery", "latest outage"}[i]
		chunk.NextAttempt = now.Add(time.Duration(i+1) * time.Minute)
		if applied, err := database.QueueAvailability(t.Context(), profile.ID, profile.Generation, unavailable, []store.OutboxChunk{chunk}); err != nil || !applied {
			t.Fatalf("transition=%v applied=%v err=%v", unavailable, applied, err)
		}
	}
	if err := database.DiscardChunk(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err := database.RescheduleChunk(t.Context(), old.ID, 9, retry.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	pending, err = database.PendingChunks(t.Context(), retry.Add(-time.Millisecond), 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("replacement bypassed retry delay: %v, %v", pending, err)
	}
	pending, err = database.PendingChunks(t.Context(), retry, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	got := pending[0]
	if got.ID <= old.ID || got.EventType != "outage" || got.Body != "latest outage" || got.Attempts != 4 || !got.NextAttempt.Equal(retry) {
		t.Fatalf("latest pending notice lost content or retry state: %+v", got)
	}
}

func TestPendingChunksNormalizesExistingAvailabilityBacklog(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovered", true: "unavailable"}[unavailable], func(t *testing.T) {
			database, profile := openAvailabilityStore(t, filepath.Join(t.TempDir(), "alerts.db"))
			now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
			retry := now.Add(time.Hour)
			if _, err := database.DB().ExecContext(t.Context(), `UPDATE profiles SET generation=3, outage_notified=? WHERE id=?`, unavailable, profile.ID); err != nil {
				t.Fatal(err)
			}
			for i, event := range []string{"outage", "outage-recovery", "outage"} {
				if _, err := database.DB().ExecContext(t.Context(), `INSERT INTO outbox_chunks
					(profile_id,revision,generation,event_type,line_ids,body,chunk_order,chunk_total,candidate_states,attempts,next_attempt_at,created_at)
					VALUES (?,1,?,?,?,?,1,1,'[]',?,?,?)`, profile.ID, i+1, event, "null", event, i+1, retry.Add(-time.Duration(i)*time.Minute).UnixMilli(), now.UnixMilli()); err != nil {
					t.Fatal(err)
				}
			}
			pending, err := database.PendingChunks(t.Context(), now, 10)
			if err != nil || len(pending) != 0 {
				t.Fatalf("backlog bypassed retry: %v, %v", pending, err)
			}
			pending, err = database.PendingChunks(t.Context(), retry, 10)
			wantEvent, wantGeneration := "outage-recovery", int64(2)
			if unavailable {
				wantEvent, wantGeneration = "outage", 3
			}
			if err != nil || len(pending) != 1 {
				t.Fatalf("backlog=%v err=%v", pending, err)
			}
			if got := pending[0]; got.EventType != wantEvent || got.Generation != wantGeneration || got.Attempts != 3 || !got.NextAttempt.Equal(retry) {
				t.Fatalf("wrong surviving notice: %+v", got)
			}
		})
	}
}
