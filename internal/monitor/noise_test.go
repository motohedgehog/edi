package monitor_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/monitor"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/tfl"
)

func TestAlertNoiseTimelineHasThreeSendsAndTwoEdits(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"northern", "metropolitan"})
	start := time.Date(2026, 1, 5, 7, 23, 0, 0, time.UTC)
	clock := &mutableClock{at: start}
	source := &changingStatusSource{}
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
	northern := disruptionSnapshot(start, "northern").Lines["northern"]
	metro := tfl.LineStatus{LineID: "metropolitan", DisplayName: "Metropolitan", Good: true, Fingerprint: "good", Statuses: []tfl.NormalizedStatus{{Severity: 10, Description: "Good Service", Known: true}}}
	for i, offset := range []time.Duration{0, 91 * time.Minute, 92 * time.Minute, 96 * time.Minute, 121 * time.Minute} {
		clock.at = start.Add(offset)
		if i > 0 {
			metro.Good = false
			metro.Fingerprint = []string{"", "first", "second", "third", "severity"}[i]
			metro.Statuses = []tfl.NormalizedStatus{{Severity: 5, Description: "Minor delays", Reason: metro.Fingerprint, Known: true}}
		}
		if i == 4 {
			metro.Statuses = append(metro.Statuses, tfl.NormalizedStatus{Severity: 9, Description: "Reduced service", Known: true})
		}
		source.snapshot = tfl.Snapshot{ObservedAt: clock.at, Lines: map[string]tfl.LineStatus{"northern": northern, "metropolitan": metro}}
		if _, err := service.Cycle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if notifier.calls != 3 || len(notifier.edits) != 2 {
		t.Fatalf("sends=%d edits=%v", notifier.calls, notifier.edits)
	}
	if notifier.edits[0] != 2 || notifier.edits[1] != 2 {
		t.Fatalf("line edit targets=%v", notifier.edits)
	}
	states, err := database.GetMonitorStates(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if states["northern"].MessageID != 1 || !states["northern"].SentAt.Equal(start) || states["metropolitan"].MessageID != 3 {
		t.Fatalf("independent receipts=%+v", states)
	}
}

func TestFailedEditsRetryLatestContentAndCancelObsoleteWork(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := &mutableClock{at: start}
	source := &changingStatusSource{snapshot: disruptionSnapshot(start, "central")}
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
	cycle := func() {
		t.Helper()
		source.snapshot.ObservedAt = clock.at
		if _, err := service.Cycle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	change := func(fp string) {
		line := source.snapshot.Lines["central"]
		line.Fingerprint = fp
		line.Statuses[0].Reason = fp
		source.snapshot.Lines["central"] = line
	}
	cycle()
	notifier.editErr = errors.New("temporary failure")
	clock.at = start.Add(time.Minute)
	change("first edit")
	cycle()
	pending, err := database.PendingChunks(ctx, clock.at.Add(time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatal("retry ran too early")
	}
	clock.at = clock.at.Add(time.Second)
	change("newest edit")
	cycle()
	if len(notifier.edits) != 1 {
		t.Fatal("replacement reset retry delay")
	}
	notifier.editErr = nil
	clock.at = clock.at.Add(time.Second)
	cycle()
	if notifier.calls != 1 || len(notifier.edits) != 2 || !strings.Contains(notifier.messages[len(notifier.messages)-1], "newest edit") {
		t.Fatalf("delivery=%+v", notifier)
	}
	notifier.editErr = errors.New("temporary failure")
	clock.at = clock.at.Add(time.Minute)
	change("obsolete")
	cycle()
	clock.at = clock.at.Add(time.Second)
	change("newest edit")
	cycle()
	pending, err = database.PendingChunks(ctx, clock.at.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatal("obsolete operation not canceled")
	}
}

func TestRestartRetainsOriginalEditClock(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "bot.db")
	database, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := &mutableClock{at: start}
	source := &changingStatusSource{snapshot: disruptionSnapshot(start, "central")}
	notifier := &fakeNotifier{}
	options := monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock}
	service := monitor.NewService(database, source, notifier, options)
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	service = monitor.NewService(database, source, notifier, options)
	clock.at = start.Add(29 * time.Minute)
	source.snapshot.ObservedAt = clock.at
	line := source.snapshot.Lines["central"]
	line.Fingerprint = "after restart"
	source.snapshot.Lines["central"] = line
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 1 || len(notifier.edits) != 1 || notifier.edits[0] != 1 {
		t.Fatalf("restart delivery=%+v", notifier)
	}
	clock.at = start.Add(30 * time.Minute)
	source.snapshot.ObservedAt = clock.at
	line.Fingerprint = "at cap"
	source.snapshot.Lines["central"] = line
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 2 {
		t.Fatal("restart reset clock")
	}
}

func TestDeletedTargetReplacementStartsNewClock(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := &mutableClock{at: start}
	source := &changingStatusSource{snapshot: disruptionSnapshot(start, "central")}
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	clock.at = start.Add(time.Minute)
	source.snapshot.ObservedAt = clock.at
	line := source.snapshot.Lines["central"]
	line.Fingerprint = "deleted target update"
	source.snapshot.Lines["central"] = line
	notifier.editErr = monitor.ErrEditTargetUnavailable
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 2 || len(notifier.edits) != 1 {
		t.Fatalf("replacement=%+v", notifier)
	}
	notifier.editErr = nil
	clock.at = clock.at.Add(time.Minute)
	source.snapshot.ObservedAt = clock.at
	line.Fingerprint = "edit replacement"
	source.snapshot.Lines["central"] = line
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 2 || notifier.edits[1] != 2 {
		t.Fatalf("replacement target=%+v", notifier)
	}
	states, err := database.GetMonitorStates(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !states["central"].SentAt.Equal(start.Add(time.Minute)) {
		t.Fatal("replacement clock incorrect")
	}
}

func TestDeliveryRechecksClockForEachLine(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central", "northern"})
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := &mutableClock{at: start}
	source := &changingStatusSource{snapshot: disruptionSnapshot(start, "central", "northern")}
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	clock.at = start.Add(29*time.Minute + 59*time.Second)
	source.snapshot.ObservedAt = clock.at
	for id, line := range source.snapshot.Lines {
		line.Fingerprint = "updated"
		source.snapshot.Lines[id] = line
	}
	notifier.onEdit = func() { clock.at = clock.at.Add(2 * time.Second) }
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 3 || len(notifier.edits) != 1 {
		t.Fatalf("expired second edit was not converted: sends=%d edits=%v", notifier.calls, notifier.edits)
	}
}

func TestDeletedTargetDoesNotSendReplacementAfterPause(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := &mutableClock{at: start}
	source := &changingStatusSource{snapshot: disruptionSnapshot(start, "central")}
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	clock.at = start.Add(time.Minute)
	source.snapshot.ObservedAt = clock.at
	line := source.snapshot.Lines["central"]
	line.Fingerprint = "updated"
	source.snapshot.Lines["central"] = line
	notifier.onEdit = func() {
		if _, _, err := database.SetProfileEnabled(ctx, profile.UserID, store.PersonaTFL, false); err != nil {
			t.Fatal(err)
		}
	}
	notifier.editErr = monitor.ErrEditTargetUnavailable
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 1 {
		t.Fatal("sent replacement after pause")
	}
}

func TestSeverityRecoveryAndUnknownTransitionsSendNewMessages(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := &mutableClock{at: start}
	source := &changingStatusSource{}
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
	for i, state := range []struct {
		good, unknown bool
		codes         []int
	}{
		{good: true, codes: []int{10}}, {codes: []int{5}}, {codes: []int{5, 9}}, {codes: []int{9, 5, 5}},
		{good: true, codes: []int{10}}, {unknown: true, codes: []int{0}}, {good: true, codes: []int{10}},
	} {
		clock.at = start.Add(time.Duration(i) * time.Minute)
		line := tfl.LineStatus{LineID: "central", DisplayName: "Central", Good: state.good, Unknown: state.unknown, Fingerprint: string(rune('a' + i))}
		for _, code := range state.codes {
			line.Statuses = append(line.Statuses, tfl.NormalizedStatus{Severity: code, Description: "Status", Known: !state.unknown})
		}
		source.snapshot = tfl.Snapshot{ObservedAt: clock.at, Lines: map[string]tfl.LineStatus{"central": line}}
		if _, err := service.Cycle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Initial healthy state is silent; reordering/duplicates keep the severity set.
	if notifier.calls != 5 || len(notifier.edits) != 1 {
		t.Fatalf("sends=%d edits=%v", notifier.calls, notifier.edits)
	}
}

func TestPendingEditBecomesSendWhenRetryReachesTimeCap(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := &mutableClock{at: start}
	source := &changingStatusSource{snapshot: disruptionSnapshot(start, "central")}
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock})
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	clock.at = start.Add(29*time.Minute + 59*time.Second)
	source.snapshot.ObservedAt = clock.at
	line := source.snapshot.Lines["central"]
	line.Fingerprint = "new description"
	source.snapshot.Lines["central"] = line
	notifier.editErr = errors.New("temporary")
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	clock.at = start.Add(30*time.Minute + time.Second)
	source.err = errors.New("TfL temporarily unavailable")
	if _, err := service.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 2 || len(notifier.edits) != 1 {
		t.Fatalf("retry failed to convert to send: %+v", notifier)
	}
}
