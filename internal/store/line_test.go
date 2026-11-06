package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/store"
)

func TestLineReconciliationAndReceiptsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alerts.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err := db.SaveProfile(ctx, store.Profile{UserID: 42, Enabled: true, AllDay: true, Lines: []string{"central"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	chunk := store.OutboxChunk{Body: "first", Format: message.FormatHTML, Operation: "send", NextAttempt: now, Candidates: []store.CandidateState{{LineID: "central", Generation: p.Generation, WindowInstance: "window", Fingerprint: "a", SeverityKey: "9", ObservedAt: now.UnixMilli()}}}
	reconcile := func(c *store.OutboxChunk) {
		t.Helper()
		if err := db.ReconcileLine(ctx, c, p.ID, p.Generation, "window", "central"); err != nil {
			t.Fatal(err)
		}
	}
	reconcile(&chunk)
	pending, err := db.PendingChunks(ctx, now, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	old := pending[0]
	retry := now.Add(time.Minute)
	if err := db.RescheduleChunk(ctx, old.ID, 1, retry); err != nil {
		t.Fatal(err)
	}
	chunk.Body = "newest"
	chunk.Candidates[0].Fingerprint = "b"
	reconcile(&chunk)
	if err := db.CompleteLine(ctx, old, 11, now); err != nil {
		t.Fatal(err)
	}
	states, err := db.GetMonitorStates(ctx, p.ID)
	if err != nil || len(states) != 0 {
		t.Fatalf("stale completion wrote states=%+v err=%v", states, err)
	}
	pending, err = db.PendingChunks(ctx, now, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("replacement reset retry: %+v %v", pending, err)
	}
	pending, err = db.PendingChunks(ctx, retry, 10)
	if err != nil || len(pending) != 1 || pending[0].Body != "newest" || pending[0].Version <= old.Version {
		t.Fatalf("replacement=%+v err=%v", pending, err)
	}
	latest := pending[0]
	chunk.Candidates[0].ObservedAt = retry.UnixMilli()
	chunk.Body = "same state at a later observation"
	reconcile(&chunk)
	pending, err = db.PendingChunks(ctx, retry, 10)
	if err != nil || len(pending) != 1 || pending[0].Version <= latest.Version || pending[0].Body != "same state at a later observation" || !pending[0].NextAttempt.Equal(retry) {
		t.Fatalf("unchanged state did not refresh observation while preserving retry: %+v %v", pending, err)
	}
	if err := db.CompleteLine(ctx, pending[0], 12, retry); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	states, err = db.GetMonitorStates(ctx, p.ID)
	state := states["central"]
	if err != nil || state.MessageID != 12 || !state.SentAt.Equal(retry) || state.Fingerprint != "b" || state.SeverityKey != "9" {
		t.Fatalf("receipt=%+v err=%v", state, err)
	}
	chunk.Body = "edit"
	chunk.Operation = "edit"
	chunk.MessageID = 12
	chunk.Candidates[0].Fingerprint = "c"
	chunk.NextAttempt = retry
	reconcile(&chunk)
	pending, err = db.PendingChunks(ctx, retry, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("edit=%+v err=%v", pending, err)
	}
	if err := db.CompleteLine(ctx, pending[0], 12, retry); err != nil {
		t.Fatal(err)
	}
	states, err = db.GetMonitorStates(ctx, p.ID)
	if err != nil || states["central"].Fingerprint != "c" || !states["central"].SentAt.Equal(retry) {
		t.Fatalf("edit receipt=%+v %v", states, err)
	}
	chunk.Candidates[0].Fingerprint = "d"
	reconcile(&chunk)
	reconcile(nil)
	pending, err = db.PendingChunks(ctx, retry, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("cancel=%+v %v", pending, err)
	}
}

func TestLineCompletionRejectsChangedGenerationAndRollsBackReceiptFailure(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err := db.SaveProfile(ctx, store.Profile{UserID: 42, Enabled: true, AllDay: true, Lines: []string{"central"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	chunk := store.OutboxChunk{Body: "alert", NextAttempt: now, Candidates: []store.CandidateState{{LineID: "central", Generation: p.Generation, WindowInstance: "window", Fingerprint: "a"}}}
	if err := db.ReconcileLine(ctx, &chunk, p.ID, p.Generation, "window", "central"); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingChunks(ctx, now, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v %v", pending, err)
	}
	if _, err := db.DB().ExecContext(ctx, `CREATE TRIGGER reject_receipt BEFORE INSERT ON monitor_states BEGIN SELECT RAISE(ABORT, 'test'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteLine(ctx, pending[0], 12, now); err == nil {
		t.Fatal("wanted receipt failure")
	}
	retained, err := db.PendingChunks(ctx, now, 10)
	if err != nil || len(retained) != 1 {
		t.Fatalf("failed transaction lost pending work: %+v %v", retained, err)
	}
	if _, err := db.DB().ExecContext(ctx, `DROP TRIGGER reject_receipt`); err != nil {
		t.Fatal(err)
	}
	p.Lines = []string{"northern"}
	if _, err := db.SaveProfile(ctx, p, p.Revision); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteLine(ctx, pending[0], 12, now); err != nil {
		t.Fatal(err)
	}
	states, err := db.GetMonitorStates(ctx, p.ID)
	if err != nil || len(states) != 0 {
		t.Fatalf("stale generation receipt=%+v %v", states, err)
	}
}

func TestRetiredEditTargetSurvivesRestartAndRejectsStaleResponse(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alerts.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err := db.SaveProfile(ctx, store.Profile{UserID: 42, Enabled: true, AllDay: true, Lines: []string{"central"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	state := store.MonitorState{ProfileID: p.ID, LineID: "central", Generation: p.Generation, WindowInstance: "window", Fingerprint: "a", SeverityKey: "9", MessageID: 12, SentAt: now}
	if err := db.SaveMonitorStates(ctx, []store.MonitorState{state}); err != nil {
		t.Fatal(err)
	}
	chunk := store.OutboxChunk{Body: "edit", Operation: "edit", MessageID: 12, NextAttempt: now, Candidates: []store.CandidateState{{LineID: "central", Generation: p.Generation, WindowInstance: "window", Fingerprint: "b", SeverityKey: "9"}}}
	if err := db.ReconcileLine(ctx, &chunk, p.ID, p.Generation, "window", "central"); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingChunks(ctx, now, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v %v", pending, err)
	}
	stale := pending[0]
	if err := db.RetireEditTarget(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if err := db.RescheduleChunk(ctx, stale.ID, 1, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	states, err := db.GetMonitorStates(ctx, p.ID)
	if err != nil || states["central"].MessageID != 0 || states["central"].Fingerprint != "a" || states["central"].SeverityKey != "9" || !states["central"].SentAt.Equal(now) {
		t.Fatalf("retired state=%+v %v", states, err)
	}
	pending, err = db.PendingChunks(ctx, now.Add(time.Minute), 10)
	if err != nil || len(pending) != 1 || pending[0].Operation != "send" || pending[0].MessageID != 0 || pending[0].Version != stale.Version {
		t.Fatalf("retired operation=%+v %v", pending, err)
	}
	if err := db.CompleteLine(ctx, pending[0], 13, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	chunk.MessageID = 13
	chunk.Candidates[0].Fingerprint = "c"
	chunk.NextAttempt = now.Add(time.Minute)
	if err := db.ReconcileLine(ctx, &chunk, p.ID, p.Generation, "window", "central"); err != nil {
		t.Fatal(err)
	}
	if err := db.RetireEditTarget(ctx, stale); err != nil {
		t.Fatal(err)
	}
	states, err = db.GetMonitorStates(ctx, p.ID)
	if err != nil || states["central"].MessageID != 13 {
		t.Fatalf("stale retirement erased new receipt: %+v %v", states, err)
	}
}
