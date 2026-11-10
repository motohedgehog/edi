package monitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/monitor"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/tfl"
)

func TestRecoveryNeedsSustainedGoodObservations(t *testing.T) {
	database, ctx := openTestStore(t)
	profile := saveEnabledProfile(ctx, t, database, []string{"central"})
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	clock := &mutableClock{at: start}
	source := &changingStatusSource{}
	notifier := &fakeNotifier{}
	service := monitor.NewService(database, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock, RecoveryConfirmationPeriod: 3 * time.Minute})
	for i, good := range []bool{false, true, false, true, true, true, true, true} {
		clock.at = start.Add(time.Duration(i) * time.Minute)
		source.snapshot = disruptionSnapshot(clock.at, "central")
		if good {
			source.snapshot = goodSnapshot(clock.at)
		}
		if _, err := service.Cycle(ctx); err != nil {
			t.Fatal(err)
		}
		want := 1
		if i >= 6 {
			want = 2
		}
		if notifier.calls != want {
			t.Fatalf("minute %d: sends=%d want=%d", i, notifier.calls, want)
		}
		states, err := database.GetMonitorStates(ctx, profile.ID)
		if err != nil {
			t.Fatal(err)
		}
		if states["central"].Good != (i >= 6) {
			t.Fatalf("minute %d: pending recovery replaced disruption baseline", i)
		}
	}
}

func TestRecoveryConfirmationResets(t *testing.T) {
	for _, reset := range []string{"disruption", "unknown", "poll failure", "restart", "pause", "pause between polls", "generation"} {
		t.Run(reset, func(t *testing.T) {
			database, ctx := openTestStore(t)
			profile := saveEnabledProfile(ctx, t, database, []string{"central"})
			start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
			clock := &mutableClock{at: start}
			source := &changingStatusSource{snapshot: disruptionSnapshot(start, "central")}
			notifier := &fakeNotifier{}
			opts := monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock, RecoveryConfirmationPeriod: 3 * time.Minute}
			service := monitor.NewService(database, source, notifier, opts)
			cycle := func() {
				t.Helper()
				if _, err := service.Cycle(ctx); err != nil {
					t.Fatal(err)
				}
			}
			cycle()
			clock.at = start.Add(time.Minute)
			source.snapshot = goodSnapshot(clock.at)
			cycle()
			clock.at = start.Add(2 * time.Minute)
			switch reset {
			case "disruption":
				source.snapshot = disruptionSnapshot(clock.at, "central")
				cycle()
			case "unknown":
				source.snapshot = tfl.Snapshot{ObservedAt: clock.at, Lines: map[string]tfl.LineStatus{}}
				cycle()
			case "poll failure":
				source.err = errors.New("upstream unavailable")
				cycle()
				source.err = nil
			case "restart":
				service = monitor.NewService(database, source, notifier, opts)
			case "pause", "pause between polls":
				if _, _, err := database.SetProfileEnabled(ctx, profile.UserID, store.PersonaTFL, false); err != nil {
					t.Fatal(err)
				}
				if reset == "pause" {
					cycle()
				}
				if _, _, err := database.SetProfileEnabled(ctx, profile.UserID, store.PersonaTFL, true); err != nil {
					t.Fatal(err)
				}
			case "generation":
				profile.Timezone = "UTC"
				updated, err := database.SaveProfile(ctx, profile, profile.Revision)
				if err != nil {
					t.Fatal(err)
				}
				profile = updated
			}
			if reset == "generation" {
				// Re-establish a delivered disruption in the replacement generation.
				source.snapshot = disruptionSnapshot(clock.at, "central")
				cycle()
			}
			before := notifier.calls
			for _, minute := range []int{3, 4, 5, 6, 7} {
				clock.at = start.Add(time.Duration(minute) * time.Minute)
				source.snapshot = goodSnapshot(clock.at)
				cycle()
				want := before
				if minute >= 6 {
					want++
				}
				if notifier.calls != want {
					t.Fatalf("minute %d: sends=%d want=%d", minute, notifier.calls, want)
				}
			}
		})
	}
}

func TestRecoveryRetriesCannotBypassReset(t *testing.T) {
	for _, reset := range []string{"poll failure", "restart", "restart next period", "disruption", "unknown"} {
		t.Run(reset, func(t *testing.T) {
			database, ctx := openTestStore(t)
			profile := saveProfileAt(ctx, t, database, store.Profile{UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, ScheduleMode: store.SchedulePeak, Timezone: "Europe/London", Lines: []string{"central"}})
			start := time.Date(2026, 1, 5, 9, 28, 0, 0, time.UTC)
			clock := &mutableClock{at: start}
			source := &changingStatusSource{snapshot: disruptionSnapshot(start, "central")}
			notifier := &fakeNotifier{failCalls: map[int]bool{2: true}}
			opts := monitor.ServiceOptions{AllowedUsers: []int64{profile.UserID}, Clock: clock, RecoveryConfirmationPeriod: time.Minute}
			service := monitor.NewService(database, source, notifier, opts)
			cycle := func() {
				t.Helper()
				if _, err := service.Cycle(ctx); err != nil {
					t.Fatal(err)
				}
			}
			cycle()
			for _, minute := range []int{1, 2} {
				clock.at = start.Add(time.Duration(minute) * time.Minute)
				source.snapshot = goodSnapshot(clock.at)
				cycle()
			}
			if notifier.calls != 2 {
				t.Fatalf("recovery not attempted outside schedule: %+v", notifier)
			}
			clock.at = clock.at.Add(2 * time.Second)
			switch reset {
			case "poll failure":
				source.err = errors.New("upstream unavailable")
			case "restart":
				service = monitor.NewService(database, source, notifier, opts)
				source.snapshot = goodSnapshot(clock.at)
			case "restart next period":
				clock.at = time.Date(2026, 1, 5, 16, 0, 0, 0, time.UTC)
				service = monitor.NewService(database, source, notifier, opts)
				source.snapshot = goodSnapshot(clock.at)
			case "disruption":
				source.snapshot = disruptionSnapshot(clock.at, "central")
			case "unknown":
				source.snapshot = tfl.Snapshot{ObservedAt: clock.at, Lines: map[string]tfl.LineStatus{}}
			}
			cycle()
			if notifier.calls != 2 {
				t.Fatalf("stale recovery retried after %s", reset)
			}
			pending, err := database.PendingChunks(ctx, clock.at.Add(time.Hour), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("stale recovery retained after %s: %+v", reset, pending)
			}
			source.err = nil
			clock.at = clock.at.Add(time.Second)
			source.snapshot = goodSnapshot(clock.at)
			cycle()
			if notifier.calls != 2 {
				t.Fatal("first good observation after reset retried recovery")
			}
			clock.at = clock.at.Add(time.Minute)
			source.snapshot = goodSnapshot(clock.at)
			cycle()
			cycle()
			if notifier.calls != 3 {
				t.Fatalf("confirmed recovery should deliver once: %+v", notifier)
			}
		})
	}
}

type failingMonitorStates struct {
	*store.Store
	profileID int64
	err       error
}

func (r *failingMonitorStates) GetMonitorStates(ctx context.Context, profileID int64) (map[string]store.MonitorState, error) {
	if r.err != nil && profileID == r.profileID {
		return nil, r.err
	}
	return r.Store.GetMonitorStates(ctx, profileID)
}

func TestRecoveryConfirmationResetsAfterPreparationFailure(t *testing.T) {
	for _, test := range []struct {
		name                      string
		unknown, multipleProfiles bool
	}{
		{name: "disruption"},
		{name: "unknown", unknown: true},
		{name: "earlier profile disruption", multipleProfiles: true},
		{name: "earlier profile unknown", unknown: true, multipleProfiles: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, ctx := openTestStore(t)
			profile := saveEnabledProfile(ctx, t, database, []string{"central"})
			profiles := 1
			if test.multipleProfiles {
				saveProfileAt(ctx, t, database, store.Profile{UserID: 43, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true, Timezone: "Europe/London", Lines: []string{"central"}})
				profiles++
			}
			repo := &failingMonitorStates{Store: database, profileID: profile.ID}
			start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
			clock := &mutableClock{at: start}
			source := &changingStatusSource{snapshot: disruptionSnapshot(start, "central")}
			notifier := &fakeNotifier{}
			service := monitor.NewService(repo, source, notifier, monitor.ServiceOptions{AllowedUsers: []int64{42, 43}, Clock: clock, RecoveryConfirmationPeriod: 3 * time.Minute})
			cycle := func() {
				t.Helper()
				if _, err := service.Cycle(ctx); err != nil {
					t.Fatal(err)
				}
			}
			cycle()
			clock.at = start.Add(time.Minute)
			source.snapshot = goodSnapshot(clock.at)
			cycle()
			clock.at = start.Add(2 * time.Minute)
			source.snapshot = disruptionSnapshot(clock.at, "central")
			if test.unknown {
				source.snapshot = tfl.Snapshot{ObservedAt: clock.at}
			}
			storeErr := errors.New("monitor state unavailable")
			repo.err = storeErr
			if _, err := service.Cycle(ctx); !errors.Is(err, storeErr) {
				t.Fatalf("preparation error lost: %v", err)
			}
			repo.err = nil
			for _, minute := range []int{4, 6, 7, 8} {
				clock.at = start.Add(time.Duration(minute) * time.Minute)
				source.snapshot = goodSnapshot(clock.at)
				cycle()
				want := profiles
				if minute >= 7 {
					want *= 2
				}
				if notifier.calls != want {
					t.Fatalf("minute %d: sends=%d want=%d", minute, notifier.calls, want)
				}
			}
		})
	}
}
