package monitor_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/logging/logtest"
	"github.com/motohedgehog/edi/internal/monitor"
	"github.com/motohedgehog/edi/internal/store"
)

type failingCompletion struct {
	*store.Store
	err error
}

func (s failingCompletion) CompleteLine(context.Context, store.OutboxChunk, int, time.Time) error {
	return s.err
}

func TestDeliveryCompletionRequiresPersistence(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	observed := time.Date(2026, time.January, 1, 12, 1, 0, 0, time.UTC)
	logger, logs := logtest.New(slog.LevelDebug)
	persistenceErr := errors.New("private-database-marker")
	notifier := &fakeNotifier{}
	service := monitor.NewService(failingCompletion{Store: database, err: persistenceErr},
		fakeStatusSource{snapshot: disruptionSnapshot(observed, "central")}, notifier,
		monitor.ServiceOptions{Logger: logger, AllowedUsers: []int64{profile.UserID}, Clock: fixedMonitorClock{at: observed}})
	result, err := service.Cycle(ctx)
	if !errors.Is(err, persistenceErr) || result.Delivered != 0 || notifier.calls != 1 {
		t.Fatalf("result=%+v err=%v sends=%d", result, err, notifier.calls)
	}
	if pending, err := database.PendingChunks(ctx, observed, 10); err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	logs.AssertPrivate(t, "Minor delays", "Trains are slow", "<b>")
}

func TestDeliveryKeepsStatusDetailsPrivate(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	observed := time.Date(2026, time.January, 1, 12, 1, 0, 0, time.UTC)
	logger, logs := logtest.New(slog.LevelDebug)
	source := &changingStatusSource{snapshot: disruptionSnapshot(observed, "central")}
	notifier := &fakeNotifier{editErr: monitor.ErrEditTargetUnavailable}
	service := monitor.NewService(database, source, notifier,
		monitor.ServiceOptions{Logger: logger, AllowedUsers: []int64{profile.UserID}, Clock: fixedMonitorClock{at: observed}})
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	line := source.snapshot.Lines["central"]
	line.Fingerprint = "private-fingerprint-marker"
	line.Statuses[0].Reason = "private-reason-marker"
	source.snapshot.Lines["central"] = line
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	// A further change cannot be edited when the target failed; its failed send stays pending.
	line.Fingerprint = "another-private-fingerprint"
	source.snapshot.Lines["central"] = line
	notifier.failCalls = map[int]bool{3: true}
	if result, err := service.Cycle(ctx); err != nil || result.Delivered != 0 {
		t.Fatalf("retry result=%+v err=%v", result, err)
	}
	logs.AssertPrivate(t, "private-fingerprint-marker", "private-reason-marker", "Minor delays", "Trains are slow", "<b>")
}
