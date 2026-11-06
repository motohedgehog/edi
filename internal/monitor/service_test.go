package monitor_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/monitor"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/tfl"
)

type steppedClock struct {
	times []time.Time
	index int
}

func (c *steppedClock) Now() time.Time {
	if c.index >= len(c.times) {
		return c.times[len(c.times)-1]
	}
	now := c.times[c.index]
	c.index++
	return now
}

type fakeStatusSource struct {
	snapshot tfl.Snapshot
}

func (f fakeStatusSource) Status(context.Context) (tfl.Snapshot, error) {
	return f.snapshot, nil
}

type changingStatusSource struct {
	snapshot tfl.Snapshot
	err      error
}

func (f *changingStatusSource) Status(context.Context) (tfl.Snapshot, error) {
	return f.snapshot, f.err
}

type fakeNotifier struct {
	failCalls map[int]bool
	calls     int
	messages  []string
	formats   []message.Format
	edits     []int
	editErr   error
	onEdit    func()
}

func (f *fakeNotifier) Send(_ context.Context, _ int64, body string, format message.Format) (int, error) {
	f.calls++
	f.messages = append(f.messages, body)
	f.formats = append(f.formats, format)
	if f.failCalls[f.calls] {
		return 0, errors.New("send failed")
	}
	return f.calls, nil
}

func (f *fakeNotifier) Edit(_ context.Context, _ int64, id int, body string, format message.Format) error {
	if f.onEdit != nil {
		f.onEdit()
	}
	f.edits = append(f.edits, id)
	f.messages = append(f.messages, body)
	f.formats = append(f.formats, format)
	return f.editErr
}

func openTestStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "edi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, ctx
}

func saveEnabledProfile(ctx context.Context, t *testing.T, database *store.Store, lines []string) store.Profile {
	t.Helper()
	profile, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true,
		Timezone: "Europe/London", Lines: lines,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func disruptionSnapshot(observed time.Time, ids ...string) tfl.Snapshot {
	lines := make(map[string]tfl.LineStatus, len(ids))
	for _, id := range ids {
		lines[id] = tfl.LineStatus{
			LineID: id, DisplayName: id, Fingerprint: id + "-disruption",
			Statuses: []tfl.NormalizedStatus{{Severity: 5, Description: "Minor delays", Reason: "Trains are slow", Known: true}},
		}
	}
	return tfl.Snapshot{ObservedAt: observed, Lines: lines}
}

func fixedCatalog(lines ...tfl.Line) func() []tfl.Line {
	return func() []tfl.Line { return append([]tfl.Line(nil), lines...) }
}

func TestServiceCycleDeliversNewAlert(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	observed := time.Date(2026, time.January, 1, 12, 1, 0, 0, time.UTC)
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, fakeStatusSource{snapshot: disruptionSnapshot(observed, "central")}, notifier, monitor.ServiceOptions{
		Catalog:      fixedCatalog(tfl.Line{ID: "central", DisplayName: "Central"}),
		AllowedUsers: []int64{profile.UserID},
		Clock:        &steppedClock{times: []time.Time{observed.Add(-time.Minute), observed.Add(time.Minute)}},
	})

	result, err := service.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.AlertsCreated != 1 || result.Delivered != 1 {
		t.Fatalf("unexpected cycle result: %+v", result)
	}
	if len(notifier.messages) != 1 {
		t.Fatalf("got %d notifier messages, want 1", len(notifier.messages))
	}
	if notifier.formats[0] != message.FormatHTML {
		t.Fatalf("format = %v, want HTML", notifier.formats[0])
	}
	pending, err := database.PendingChunks(ctx, observed.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("outbox was not emptied: %+v", pending)
	}
	states, err := database.GetMonitorStates(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("monitor state was not committed: %+v", states)
	}
}

func TestServiceReadsOneCurrentCatalogSnapshotPerCycle(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	observed := time.Date(2026, time.January, 1, 12, 1, 0, 0, time.UTC)
	var catalogReads int
	currentCatalog := []tfl.Line{{ID: "central", DisplayName: "Central Fresh"}}
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, fakeStatusSource{snapshot: tfl.Snapshot{ObservedAt: observed}}, notifier, monitor.ServiceOptions{
		Catalog: func() []tfl.Line {
			catalogReads++
			return append([]tfl.Line(nil), currentCatalog...)
		},
		AllowedUsers: []int64{profile.UserID},
		Clock:        fixedMonitorClock{at: observed},
	})
	result, err := service.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.AlertsCreated != 1 || catalogReads != 1 || len(notifier.messages) != 1 {
		t.Fatalf("cycle result=%+v catalog reads=%d messages=%v", result, catalogReads, notifier.messages)
	}
	if !strings.Contains(notifier.messages[0], "Central Fresh") {
		t.Fatalf("monitor used stale catalog name: %q", notifier.messages[0])
	}
}

func TestServiceCycleKeepsMixedLineOutcomesIndependent(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"victoria", "northern", "metropolitan", "central"})
	observed := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	window := fmt.Sprintf("%d:all-day", profile.Generation)
	for _, id := range []string{"central", "northern"} {
		if err := database.SaveMonitorStates(ctx, []store.MonitorState{{
			ProfileID: profile.ID, LineID: id, Generation: profile.Generation, WindowInstance: window,
			Fingerprint: "good", Good: true, ObservedAt: observed.Add(-time.Minute),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	obsolete := store.OutboxChunk{
		Body: "obsolete northern disruption", Format: message.FormatHTML, Operation: "send", NextAttempt: observed,
		Candidates: []store.CandidateState{{LineID: "northern", Generation: profile.Generation,
			WindowInstance: window, Fingerprint: "obsolete", ObservedAt: observed.UnixMilli()}},
	}
	if err := database.ReconcileLine(ctx, &obsolete, profile.ID, profile.Generation, window, "northern"); err != nil {
		t.Fatal(err)
	}
	snapshot := disruptionSnapshot(observed, "central")
	snapshot.Lines["northern"] = goodSnapshot(observed).Lines["central"]
	northern := snapshot.Lines["northern"]
	northern.LineID, northern.DisplayName = "northern", "Northern"
	snapshot.Lines["northern"] = northern
	snapshot.Lines["metropolitan"] = tfl.LineStatus{
		LineID: "metropolitan", DisplayName: "Metropolitan", Fingerprint: "metro-good", Good: true,
		Statuses: []tfl.NormalizedStatus{{Severity: 10, Description: "Good Service", Known: true}},
	}
	snapshot.Lines["victoria"] = tfl.LineStatus{
		LineID: "victoria", DisplayName: "Victoria", Fingerprint: "victoria-unknown", Unknown: true,
		Statuses: []tfl.NormalizedStatus{{Description: "Unknown", Reason: "Victoria data unavailable"}},
	}
	opts := monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: fixedMonitorClock{at: observed}}
	service := monitor.NewService(database, fakeStatusSource{snapshot: snapshot}, nil, opts)
	result, err := service.Cycle(ctx)
	if err != nil || result.AlertsCreated != 2 || result.Delivered != 0 {
		t.Fatalf("preparation result=%+v err=%v", result, err)
	}
	pending, err := database.PendingChunks(ctx, observed, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	kinds := map[string]string{"central": "disruption", "victoria": "unknown"}
	for _, chunk := range pending {
		if len(chunk.Candidates) != 1 || len(chunk.LineIDs) != 1 {
			t.Fatalf("line work lost its candidate: %+v", chunk)
		}
		candidate := chunk.Candidates[0]
		line, ok := snapshot.Lines[candidate.LineID]
		if !ok || kinds[candidate.LineID] == "" || chunk.LineIDs[0] != candidate.LineID ||
			candidate.Fingerprint != line.Fingerprint || candidate.SeverityKey != tfl.SeverityKey(line) ||
			candidate.Good != line.Good || candidate.Unknown != line.Unknown {
			t.Fatalf("message and candidate mismatch: %+v", chunk)
		}
		body, err := monitor.RenderAutomaticAlert(monitor.AlertLine{
			LineID: line.LineID, DisplayName: line.DisplayName, Kind: kinds[line.LineID], Statuses: line.Statuses,
		}, observed, profile.Timezone)
		if err != nil || chunk.Body != body.Body || chunk.Format != body.Format {
			t.Fatalf("wrong rendered message for %s: %+v err=%v", line.LineID, chunk, err)
		}
	}
	states, err := database.GetMonitorStates(ctx, profile.ID)
	if err != nil || len(states) != 3 || states["metropolitan"].Fingerprint != "metro-good" ||
		!states["metropolitan"].Good || states["metropolitan"].MessageID != 0 || states["northern"].Fingerprint != "good" {
		t.Fatalf("baselines=%+v err=%v", states, err)
	}
	notifier := &fakeNotifier{}
	service = monitor.NewService(database, fakeStatusSource{snapshot: snapshot}, notifier, opts)
	result, err = service.Cycle(ctx)
	if err != nil || result.Delivered != 2 || notifier.calls != 2 {
		t.Fatalf("delivery result=%+v notifier=%+v err=%v", result, notifier, err)
	}
	states, err = database.GetMonitorStates(ctx, profile.ID)
	if err != nil || len(states) != 4 {
		t.Fatalf("receipts=%+v err=%v", states, err)
	}
	for _, chunk := range pending {
		candidate := chunk.Candidates[0]
		receipt := states[candidate.LineID]
		if receipt.MessageID < 1 || receipt.MessageID > len(notifier.messages) ||
			notifier.messages[receipt.MessageID-1] != chunk.Body || receipt.Fingerprint != candidate.Fingerprint ||
			receipt.SeverityKey != candidate.SeverityKey || !receipt.SentAt.Equal(observed) {
			t.Fatalf("receipt belongs to a different line: %+v", receipt)
		}
	}
	pending, err = database.PendingChunks(ctx, observed, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("completed or obsolete work remains: %+v err=%v", pending, err)
	}
}

func TestServiceCycleRetriesFailedAlertWithoutDuplicateEnqueue(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	observed := time.Date(2026, time.January, 1, 12, 1, 0, 0, time.UTC)
	notifier := &fakeNotifier{failCalls: map[int]bool{1: true}}
	clock := &mutableClock{at: observed.Add(time.Minute)}
	service := monitor.NewService(database, fakeStatusSource{snapshot: disruptionSnapshot(observed, "central")}, notifier, monitor.ServiceOptions{
		Catalog:      fixedCatalog(tfl.Line{ID: "central", DisplayName: "Central"}),
		AllowedUsers: []int64{profile.UserID},
		Clock:        clock,
	})

	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := database.PendingChunks(ctx, observed.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Attempts != 1 {
		t.Fatalf("unexpected retry state: %+v", pending)
	}
	states, err := database.GetMonitorStates(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("monitor state committed after failed send: %+v", states)
	}

	clock.at = observed.Add(11 * time.Minute)
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err = database.PendingChunks(ctx, observed.Add(12*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("retry outbox was not emptied: %+v", pending)
	}
	if notifier.calls != 2 {
		t.Fatalf("got %d notifier calls, want 2", notifier.calls)
	}
	states, err = database.GetMonitorStates(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("monitor state was not committed after retry: %+v", states)
	}
}

func TestServiceSendsOutageNoticeWhenLineAlertsArePaused(t *testing.T) {
	database, ctx := openTestStore(t)
	profile, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: false, ScheduleMode: store.SchedulePeak,
		WindowStart: store.DefaultPeakStart, WindowEnd: store.DefaultPeakEnd, Timezone: "Europe/London",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	notifier := &fakeNotifier{}
	status := &changingStatusSource{err: errors.New("service unavailable")}
	service := monitor.NewService(database, status, notifier, monitor.ServiceOptions{
		AllowedUsers:     []int64{profile.UserID},
		FailureThreshold: 1,
		Clock:            fixedMonitorClock{at: time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)},
	})
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 1 || len(notifier.messages) != 1 {
		t.Fatalf("outage notice was not delivered: calls=%d messages=%v", notifier.calls, notifier.messages)
	}
	if !strings.Contains(notifier.messages[0], "unavailable") {
		t.Fatalf("unexpected outage notice: %q", notifier.messages[0])
	}
	if !strings.Contains(notifier.messages[0], "TfL updates unavailable") || notifier.formats[0] != message.FormatHTML {
		t.Fatalf("outage availability text/format = %q/%v", notifier.messages[0], notifier.formats[0])
	}

	status.err = nil
	status.snapshot = tfl.Snapshot{ObservedAt: time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)}
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 2 || !strings.Contains(notifier.messages[1], "TfL updates restored") || notifier.formats[1] != message.FormatHTML {
		t.Fatalf("recovery notice was not delivered: calls=%d messages=%v formats=%v", notifier.calls, notifier.messages, notifier.formats)
	}
}

func TestServiceDeliversLatestAvailabilityAfterTelegramFailure(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveProfileAt(ctx, t, database, store.Profile{UserID: 42, AllDay: true, Timezone: "UTC"})
	clock := &mutableClock{at: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	source := &changingStatusSource{}
	notifier := &fakeNotifier{failCalls: map[int]bool{1: true, 2: true}}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{
		AllowedUsers: []int64{profile.UserID}, FailureThreshold: 1, Clock: clock,
	})
	for _, unavailable := range []bool{true, false, true} {
		source.err = nil
		if unavailable {
			source.err = errors.New("TfL unavailable")
		}
		source.snapshot.ObservedAt = clock.at
		if _, err := service.Cycle(ctx); err != nil {
			t.Fatal(err)
		}
		clock.at = clock.at.Add(time.Minute)
	}
	if notifier.calls != 3 || !strings.Contains(notifier.messages[2], "TfL updates unavailable") || !strings.Contains(notifier.messages[2], "12:02") {
		t.Fatalf("delivered obsolete availability: calls=%d messages=%v", notifier.calls, notifier.messages)
	}
	pending, err := database.PendingChunks(ctx, clock.at, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("obsolete notices remain: %v, %v", pending, err)
	}
}

func TestServiceDropsExpiredScheduledRetry(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveProfileAt(ctx, t, database, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, ScheduleMode: store.SchedulePeak,
		WindowStart: store.DefaultPeakStart, WindowEnd: store.DefaultPeakEnd, Timezone: "Europe/London",
		Lines: []string{"central"},
	})
	location, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 5, 9, 29, 0, 0, location)
	after := time.Date(2026, 1, 5, 9, 31, 0, 0, location)
	status := &changingStatusSource{snapshot: disruptionSnapshot(start, "central")}
	notifier := &fakeNotifier{failCalls: map[int]bool{1: true}}
	clock := &mutableClock{at: start}
	service := monitor.NewService(database, status, notifier, monitor.ServiceOptions{
		Catalog:      fixedCatalog(tfl.Line{ID: "central", DisplayName: "Central"}),
		AllowedUsers: []int64{profile.UserID},
		Clock:        clock,
	})
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 1 {
		t.Fatalf("first delivery attempt count=%d, want 1", notifier.calls)
	}
	clock.at = after
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 1 {
		t.Fatalf("expired retry was delivered: calls=%d", notifier.calls)
	}
	pending, err := database.PendingChunks(ctx, after, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("expired retry remains queued: %+v", pending)
	}
}

func TestServiceDiscardsDisabledAndObsoleteStatusChunks(t *testing.T) {
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		enabled    bool
		generation int64
	}{
		{name: "disabled", enabled: false},
		{name: "obsolete generation", enabled: true, generation: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, ctx := openTestStore(t)
			profile := saveProfileAt(ctx, t, database, store.Profile{
				UserID: 42, PersonaID: store.PersonaTFL, Enabled: test.enabled, ScheduleMode: store.ScheduleAllDay,
				Timezone: "Europe/London", Lines: []string{"central"},
			})
			generation := profile.Generation
			if test.generation < 0 {
				generation--
			}
			enqueueStatusChunk(ctx, t, database, profile, generation, now.Add(time.Hour))
			notifier := &fakeNotifier{}
			snapshot := tfl.Snapshot{ObservedAt: now, Lines: map[string]tfl.LineStatus{
				"central": {LineID: "central", DisplayName: "Central", Fingerprint: "central-good", Good: true,
					Statuses: []tfl.NormalizedStatus{{Severity: tfl.GoodSeverity, Description: "Good Service", Known: true}}},
			}}
			service := monitor.NewService(database, fakeStatusSource{snapshot: snapshot}, notifier, monitor.ServiceOptions{
				AllowedUsers: []int64{profile.UserID}, Clock: fixedMonitorClock{at: now},
			})
			if _, err := service.Cycle(ctx); err != nil {
				t.Fatal(err)
			}
			if notifier.calls != 0 {
				t.Fatalf("invalid status chunk was delivered %d times", notifier.calls)
			}
			pending, err := database.PendingChunks(ctx, now, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("invalid status chunk remains queued: %+v", pending)
			}
		})
	}
}

func TestServiceReturnsChunkDiscardError(t *testing.T) {
	database, ctx := openTestStore(t)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	profile := saveProfileAt(ctx, t, database, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: false, ScheduleMode: store.ScheduleAllDay,
		Timezone: "Europe/London", Lines: []string{"central"},
	})
	enqueueStatusChunk(ctx, t, database, profile, profile.Generation, now.Add(time.Hour))
	wantErr := errors.New("discard failed")
	service := monitor.NewService(discardErrorRepository{Store: database, err: wantErr}, fakeStatusSource{snapshot: tfl.Snapshot{ObservedAt: now}}, &fakeNotifier{}, monitor.ServiceOptions{
		AllowedUsers: []int64{profile.UserID}, Clock: fixedMonitorClock{at: now},
	})
	if _, err := service.Cycle(ctx); !errors.Is(err, wantErr) {
		t.Fatalf("Cycle error = %v, want %v", err, wantErr)
	}
}

func TestServiceFiltersProfilesForActiveAndRecoveryAlerts(t *testing.T) {
	database, ctx := openTestStore(t)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	allowed := saveProfileAt(ctx, t, database, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, ScheduleMode: store.ScheduleAllDay,
		Timezone: "Europe/London", Lines: []string{"central"},
	})
	excluded := saveProfileAt(ctx, t, database, store.Profile{
		UserID: 99, PersonaID: store.PersonaTFL, Enabled: true, ScheduleMode: store.ScheduleAllDay,
		Timezone: "Europe/London", Lines: []string{"central"},
	})
	if _, err := database.DB().ExecContext(ctx, `UPDATE profiles SET outage_notified = 1, outage_generation = ? WHERE id = ?`, allowed.Generation, allowed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB().ExecContext(ctx, `UPDATE profiles SET outage_notified = 1, outage_generation = ? WHERE id = ?`, excluded.Generation, excluded.ID); err != nil {
		t.Fatal(err)
	}
	service := monitor.NewService(database, fakeStatusSource{snapshot: disruptionSnapshot(now, "central")}, nil, monitor.ServiceOptions{
		Catalog: fixedCatalog(tfl.Line{ID: "central", DisplayName: "Central"}), AllowedUsers: []int64{allowed.UserID}, Clock: fixedMonitorClock{at: now},
	})
	result, err := service.Cycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProfilesChecked != 1 || result.AlertsCreated != 2 {
		t.Fatalf("cycle result = %+v, want one active profile and two alerts", result)
	}
	pending, err := database.PendingChunks(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("got %d pending alerts, want status and recovery alerts", len(pending))
	}
	var statusAlert, recoveryAlert bool
	for _, chunk := range pending {
		if chunk.ProfileID != allowed.ID {
			t.Fatalf("alert queued for excluded profile %d", chunk.ProfileID)
		}
		switch chunk.EventType {
		case "status":
			statusAlert = true
		case "outage-recovery":
			recoveryAlert = true
		}
	}
	if !statusAlert || !recoveryAlert {
		t.Fatalf("queued events: status=%v recovery=%v", statusAlert, recoveryAlert)
	}
	allowed, ok, err := database.GetProfileByID(ctx, allowed.ID)
	if err != nil || !ok {
		t.Fatalf("load allowed profile: found=%v err=%v", ok, err)
	}
	if allowed.OutageNotified {
		t.Fatal("allowed profile outage state was not cleared after recovery")
	}
	excluded, ok, err = database.GetProfileByID(ctx, excluded.ID)
	if err != nil || !ok {
		t.Fatalf("load excluded profile: found=%v err=%v", ok, err)
	}
	if !excluded.OutageNotified {
		t.Fatal("excluded profile recovery state was modified")
	}
}

type discardErrorRepository struct {
	*store.Store
	err error
}

func (r discardErrorRepository) DiscardChunk(context.Context, store.OutboxChunk) error {
	return r.err
}

func enqueueStatusChunk(ctx context.Context, t *testing.T, database *store.Store, profile store.Profile, generation int64, expiresAt time.Time) {
	t.Helper()
	// Seed stale persisted work directly; normal status work goes through ReconcileLine.
	now := expiresAt.Add(-time.Hour).UTC()
	_, err := database.DB().ExecContext(ctx, `INSERT INTO outbox_chunks
		(profile_id, revision, generation, event_type, line_ids, body, message_format, chunk_order, chunk_total, candidate_states, next_attempt_at, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		profile.ID, profile.Revision, generation, "status", `["central"]`, "queued status", message.FormatPlainText.String(),
		1, 1, "[]", now.UnixMilli(), now.UnixMilli(), expiresAt.UTC().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
}

func TestServiceDeliversRecoveryAcrossMonitoringWindows(t *testing.T) {
	for _, tc := range []struct {
		name, mode, start, end string
		before, after          time.Time
	}{
		{name: "peak end", mode: store.SchedulePeak, before: time.Date(2026, 1, 5, 9, 29, 0, 0, time.UTC), after: time.Date(2026, 1, 5, 9, 31, 0, 0, time.UTC)},
		{name: "weekend", mode: store.SchedulePeak, before: time.Date(2026, 1, 9, 18, 59, 0, 0, time.UTC), after: time.Date(2026, 1, 10, 10, 0, 0, 0, time.UTC)},
		{name: "next period", mode: store.SchedulePeak, before: time.Date(2026, 1, 5, 9, 29, 0, 0, time.UTC), after: time.Date(2026, 1, 5, 16, 1, 0, 0, time.UTC)},
		{name: "custom end", mode: store.ScheduleCustom, start: "08:00", end: "09:00", before: time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC), after: time.Date(2026, 1, 5, 9, 2, 0, 0, time.UTC)},
		{name: "overnight", mode: store.ScheduleCustom, start: "22:00", end: "02:00", before: time.Date(2026, 1, 6, 2, 0, 0, 0, time.UTC), after: time.Date(2026, 1, 6, 2, 2, 0, 0, time.UTC)},
		{name: "delivered unknown", mode: store.SchedulePeak, before: time.Date(2026, 1, 5, 9, 29, 0, 0, time.UTC), after: time.Date(2026, 1, 5, 9, 31, 0, 0, time.UTC)},
		{name: "retired edit target", mode: store.SchedulePeak, before: time.Date(2026, 1, 5, 9, 29, 0, 0, time.UTC), after: time.Date(2026, 1, 5, 9, 31, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, ctx := openTestStore(t)
			profile := saveProfileAt(ctx, t, database, store.Profile{
				UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, ScheduleMode: tc.mode,
				WindowStart: tc.start, WindowEnd: tc.end, Timezone: "Europe/London", Lines: []string{"central"},
			})
			clock := &mutableClock{at: tc.before}
			source := &changingStatusSource{snapshot: disruptionSnapshot(tc.before, "central")}
			if tc.name == "delivered unknown" {
				source.snapshot.Lines["central"] = tfl.LineStatus{LineID: "central", Unknown: true, Fingerprint: "unknown"}
			}
			notifier := &fakeNotifier{}
			service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
			if _, err := service.Cycle(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.name == "retired edit target" {
				states, err := database.GetMonitorStates(ctx, profile.ID)
				if err != nil {
					t.Fatal(err)
				}
				old := states["central"]
				old.MessageID = 0
				if err := database.SaveMonitorStates(ctx, []store.MonitorState{old}); err != nil {
					t.Fatal(err)
				}
			}
			clock.at = tc.after
			source.snapshot = goodSnapshot(tc.after)
			for range 2 {
				if _, err := service.Cycle(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if notifier.calls != 2 || len(notifier.edits) != 0 || !strings.Contains(notifier.messages[1], "Good Service") {
				t.Fatalf("want disruption and one recovery send, got %+v", notifier)
			}
		})
	}
}

func goodSnapshot(observed time.Time) tfl.Snapshot {
	return tfl.Snapshot{ObservedAt: observed, Lines: map[string]tfl.LineStatus{
		"central": {LineID: "central", DisplayName: "Central", Good: true, Fingerprint: "good",
			Statuses: []tfl.NormalizedStatus{{Severity: 10, Description: "Good Service", Known: true}}},
	}}
}

func TestServiceRecoveryRetrySurvivesWindowEndAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "edi.db")
	database, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	profile := saveProfileAt(ctx, t, database, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, ScheduleMode: store.SchedulePeak,
		Timezone: "Europe/London", Lines: []string{"central"},
	})
	clock := &mutableClock{at: time.Date(2026, 1, 5, 9, 28, 0, 0, time.UTC)}
	source := &changingStatusSource{snapshot: disruptionSnapshot(clock.at, "central")}
	notifier := &fakeNotifier{failCalls: map[int]bool{2: true}}
	opts := monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock}
	service := monitor.NewService(database, source, notifier, opts)
	cycle := func() {
		t.Helper()
		source.snapshot.ObservedAt = clock.at
		if _, err := service.Cycle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	cycle()
	clock.at = clock.at.Add(time.Minute)
	source.snapshot = goodSnapshot(clock.at)
	cycle()
	clock.at = clock.at.Add(time.Second)
	cycle()
	if notifier.calls != 2 {
		t.Fatalf("retry delay reset: calls=%d", notifier.calls)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	service = monitor.NewService(database, source, notifier, opts)
	clock.at = time.Date(2026, 1, 5, 9, 31, 0, 0, time.UTC)
	pending, err := database.PendingChunks(ctx, clock.at, 10)
	if err != nil || len(pending) != 1 || !pending[0].ExpiresAt.IsZero() {
		t.Fatalf("recovery lost at window end: pending=%+v err=%v", pending, err)
	}
	cycle()
	cycle()
	if notifier.calls != 3 || len(notifier.edits) != 0 {
		t.Fatalf("want one recovery retry after restart, got %+v", notifier)
	}
}

func TestServiceOutsideWindowRecoveryRequiresDeliveredAlert(t *testing.T) {
	for _, name := range []string{"initial good", "unalerted disruption", "failed disruption", "historical baseline"} {
		t.Run(name, func(t *testing.T) {
			database, ctx := openTestStore(t)
			profile := saveProfileAt(ctx, t, database, store.Profile{
				UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, ScheduleMode: store.SchedulePeak,
				Timezone: "Europe/London", Lines: []string{"central"},
			})
			clock := &mutableClock{at: time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)}
			source := &changingStatusSource{snapshot: disruptionSnapshot(clock.at, "central")}
			notifier := &fakeNotifier{failCalls: map[int]bool{1: true}}
			service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
			switch name {
			case "failed disruption":
				clock.at = time.Date(2026, 1, 5, 9, 29, 0, 0, time.UTC)
				source.snapshot.ObservedAt = clock.at
				fallthrough
			case "unalerted disruption":
				if _, err := service.Cycle(ctx); err != nil {
					t.Fatal(err)
				}
			case "historical baseline":
				if err := database.SaveMonitorStates(ctx, []store.MonitorState{{ProfileID: profile.ID, LineID: "central", Generation: profile.Generation, Fingerprint: "bad", ObservedAt: clock.at}}); err != nil {
					t.Fatal(err)
				}
			}
			before := notifier.calls
			clock.at = time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
			source.snapshot = goodSnapshot(clock.at)
			if _, err := service.Cycle(ctx); err != nil {
				t.Fatal(err)
			}
			if notifier.calls != before {
				t.Fatalf("unsolicited recovery: %+v", notifier)
			}
		})
	}
}

func TestServiceCancelsPendingRecovery(t *testing.T) {
	for _, name := range []string{"disrupted", "unknown", "paused", "deselected", "new schedule", "new timezone", "disallowed"} {
		t.Run(name, func(t *testing.T) {
			database, ctx := openTestStore(t)
			profile := saveProfileAt(ctx, t, database, store.Profile{
				UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, ScheduleMode: store.SchedulePeak,
				Timezone: "Europe/London", Lines: []string{"central"},
			})
			clock := &mutableClock{at: time.Date(2026, 1, 5, 9, 29, 0, 0, time.UTC)}
			source := &changingStatusSource{snapshot: disruptionSnapshot(clock.at, "central")}
			notifier := &fakeNotifier{failCalls: map[int]bool{2: true}}
			opts := monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock}
			service := monitor.NewService(database, source, notifier, opts)
			if _, err := service.Cycle(ctx); err != nil {
				t.Fatal(err)
			}
			clock.at = time.Date(2026, 1, 5, 9, 31, 0, 0, time.UTC)
			source.snapshot = goodSnapshot(clock.at)
			if _, err := service.Cycle(ctx); err != nil {
				t.Fatal(err)
			}
			if notifier.calls != 2 {
				t.Fatalf("recovery was not attempted: %+v", notifier)
			}
			switch name {
			case "disrupted":
				source.snapshot = disruptionSnapshot(clock.at, "central")
			case "unknown":
				source.snapshot.Lines["central"] = tfl.LineStatus{LineID: "central", Unknown: true, Fingerprint: "unknown"}
			case "paused":
				profile.Enabled = false
			case "deselected":
				profile.Lines = []string{"northern"}
			case "new schedule":
				profile.ScheduleMode = store.ScheduleCustom
				profile.WindowStart, profile.WindowEnd = "06:00", "07:00"
			case "new timezone":
				profile.Timezone = "America/New_York"
			case "disallowed":
				opts.AllowedUsers = []int64{}
				service = monitor.NewService(database, source, notifier, opts)
			}
			if _, err := database.SaveProfile(ctx, profile, profile.Revision); err != nil {
				t.Fatal(err)
			}
			clock.at = clock.at.Add(time.Minute)
			source.snapshot.ObservedAt = clock.at
			if _, err := service.Cycle(ctx); err != nil {
				t.Fatal(err)
			}
			if notifier.calls != 2 {
				t.Fatalf("obsolete recovery delivered: %+v", notifier)
			}
			if name != "disallowed" {
				pending, err := database.PendingChunks(ctx, clock.at, 10)
				if err != nil || len(pending) != 0 {
					t.Fatalf("obsolete recovery retained: pending=%+v err=%v", pending, err)
				}
			}
		})
	}
}

type fixedMonitorClock struct{ at time.Time }

func (c fixedMonitorClock) Now() time.Time { return c.at }

func saveProfileAt(ctx context.Context, t *testing.T, database *store.Store, profile store.Profile) store.Profile {
	t.Helper()
	saved, err := database.SaveProfile(ctx, profile, 0)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestDescriptionChangesEditOriginalMessageUntilThirtyMinutes(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	start := time.Date(2026, 1, 5, 7, 23, 0, 0, time.UTC)
	clock := &mutableClock{at: start}
	source := &changingStatusSource{}
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
	for i, offset := range []time.Duration{0, 10 * time.Minute, 29*time.Minute + 59*time.Second, 30 * time.Minute} {
		clock.at = start.Add(offset)
		source.snapshot = disruptionSnapshot(clock.at, "central")
		line := source.snapshot.Lines["central"]
		line.Fingerprint = string(rune('a' + i))
		line.Statuses[0].Reason = line.Fingerprint
		source.snapshot.Lines["central"] = line
		if _, err := service.Cycle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if notifier.calls != 2 || len(notifier.edits) != 2 {
		t.Fatalf("sends=%d edits=%v", notifier.calls, notifier.edits)
	}
	if notifier.edits[0] != 1 || notifier.edits[1] != 1 {
		t.Fatalf("edit targets=%v", notifier.edits)
	}
	states, err := database.GetMonitorStates(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !states["central"].SentAt.Equal(start.Add(30 * time.Minute)) {
		t.Fatalf("send clock=%v", states["central"].SentAt)
	}
}

type mutableClock struct{ at time.Time }

func (c *mutableClock) Now() time.Time { return c.at }

func TestServiceCustomScheduleDeliversInRepeatedHourWithoutRepeatingUnchangedAlerts(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveProfileAt(ctx, t, database, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, ScheduleMode: store.ScheduleCustom,
		WindowStart: "00:00", WindowEnd: "01:30", Timezone: "America/New_York", Lines: []string{"central", "northern"},
	})
	clock := &mutableClock{at: time.Date(2026, 11, 1, 5, 15, 0, 0, time.UTC)}
	source := &changingStatusSource{snapshot: disruptionSnapshot(clock.at, "central", "northern")}
	northern := source.snapshot.Lines["northern"]
	northern.Good, northern.Fingerprint = true, "good"
	northern.Statuses = []tfl.NormalizedStatus{{Severity: 10, Description: "Good Service", Known: true}}
	source.snapshot.Lines["northern"] = northern
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
	if result, err := service.Cycle(ctx); err != nil || result.Delivered != 1 {
		t.Fatalf("first occurrence: result=%+v err=%v", result, err)
	}
	clock.at = clock.at.Add(30 * time.Minute)
	source.snapshot = disruptionSnapshot(clock.at, "central", "northern")
	if result, err := service.Cycle(ctx); err != nil || result.Delivered != 0 {
		t.Fatalf("inactive gap: result=%+v err=%v", result, err)
	}
	clock.at = clock.at.Add(30 * time.Minute)
	source.snapshot.ObservedAt = clock.at
	if result, err := service.Cycle(ctx); err != nil || result.Delivered != 1 {
		t.Fatalf("second occurrence: result=%+v err=%v", result, err)
	}
	if notifier.calls != 2 || !strings.Contains(notifier.messages[1], "northern") {
		t.Fatalf("want one alert per changed line across both occurrences: %+v", notifier)
	}
}

type monitorRoundTripFunc func(*http.Request) (*http.Response, error)

func (f monitorRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestServiceNewModesDisruptionUnknownAndRecovery(t *testing.T) {
	for _, line := range []tfl.Line{
		{ID: "elizabeth", DisplayName: "Elizabeth line", ModeName: "elizabeth-line"},
		{ID: "dlr", DisplayName: "DLR", ModeName: "dlr"},
		{ID: "windrush", DisplayName: "Windrush", ModeName: "overground"},
	} {
		t.Run(line.ID, func(t *testing.T) {
			database, ctx := openTestStore(t)
			saveEnabledProfile(ctx, t, database, []string{line.ID})
			mode, severity := line.ModeName, 5
			transport := monitorRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/Line/Meta/Severity" {
					return &http.Response{StatusCode: http.StatusServiceUnavailable,
						Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
				}
				body := fmt.Sprintf(`[{"id":%q,"name":%q,"modeName":%q,"lineStatuses":[{"statusSeverity":%d,"statusSeverityDescription":%q}]}]`,
					line.ID, line.DisplayName, mode, severity, map[int]string{5: "Minor delays", 10: "Good Service"}[severity])
				return &http.Response{StatusCode: http.StatusOK,
					Header: http.Header{"Content-Type": []string{"application/json"}},
					Body:   io.NopCloser(strings.NewReader(body))}, nil
			})
			client := tfl.NewClient("", time.Second,
				tfl.WithHTTPClient(&http.Client{Transport: transport}),
				tfl.WithSleep(func(context.Context, time.Duration) error { return nil }))
			if err := client.SetSeverities([]tfl.Severity{
				{ModeName: "tube", SeverityLevel: 10, Description: "Good Service"},
				{ModeName: line.ModeName, SeverityLevel: 5, Description: "Minor delays"},
				{ModeName: line.ModeName, SeverityLevel: 10, Description: "Good Service"},
			}); err != nil {
				t.Fatal(err)
			}
			notifier := &fakeNotifier{}
			service := monitor.NewService(database, client, notifier, monitor.ServiceOptions{
				AllowedUsers: []int64{42}, Catalog: fixedCatalog(line),
				Clock: monitor.RealClock{},
			})
			if _, err := service.Cycle(ctx); err != nil {
				t.Fatal(err)
			}
			if notifier.calls != 1 || !strings.Contains(notifier.messages[0], "Minor delays") {
				t.Fatalf("disruption not delivered: %v", notifier.messages)
			}
			mode, severity = "", 10
			if _, err := service.Cycle(ctx); err != nil {
				t.Fatal(err)
			}
			if notifier.calls != 2 || !strings.Contains(notifier.messages[1], "Unknown") {
				t.Fatalf("missing mode caused false recovery: %v", notifier.messages)
			}
			mode = line.ModeName
			if _, err := service.Cycle(ctx); err != nil {
				t.Fatal(err)
			}
			if notifier.calls != 3 || !strings.Contains(notifier.messages[2], "Good Service") {
				t.Fatalf("recovery not delivered: %v", notifier.messages)
			}
			if _, err := service.Cycle(ctx); err != nil {
				t.Fatal(err)
			}
			if notifier.calls != 3 {
				t.Fatal("unchanged good service sent another alert")
			}
		})
	}
}
