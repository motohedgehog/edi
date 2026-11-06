package monitor_test

import (
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/monitor"
)

func TestCustomScheduleIncludesEndMinuteAndKeepsOvernightIdentity(t *testing.T) {
	zone, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	active, _, expiry := monitor.SchedulePeriod(time.Date(2026, 1, 2, 9, 30, 59, 0, zone), monitor.ScheduleCustom, "Europe/London", "07:30", "09:30")
	if !active {
		t.Fatal("end minute was not active")
	}
	if want := time.Date(2026, 1, 2, 9, 31, 0, 0, zone); !expiry.Equal(want) {
		t.Fatalf("end expiry = %v, want %v", expiry, want)
	}
	active, _, _ = monitor.SchedulePeriod(time.Date(2026, 1, 2, 9, 31, 0, 0, zone), monitor.ScheduleCustom, "Europe/London", "07:30", "09:30")
	if active {
		t.Fatal("minute after end was active")
	}
	active, period, _ := monitor.SchedulePeriod(time.Date(2026, 1, 3, 1, 30, 0, 0, zone), monitor.ScheduleCustom, "Europe/London", "22:00", "02:00")
	if !active {
		t.Fatal("overnight window was inactive")
	}
	if period != "2026-01-02" {
		t.Fatalf("overnight period = %q, want previous day's date", period)
	}
}

func TestAllDayScheduleHasStableIdentityAndNoExpiry(t *testing.T) {
	active, period, expiry := monitor.SchedulePeriod(time.Now(), monitor.ScheduleAllDay, "UTC", "", "")
	if !active || period != "all-day" || !expiry.IsZero() {
		t.Fatalf("all-day schedule = (%v, %q, %v), want active all-day with no expiry", active, period, expiry)
	}
}

func TestWindowValidation(t *testing.T) {
	if err := monitor.ValidateWindow("10:00", "10:00", false); err == nil {
		t.Fatal("equal times accepted")
	}
	if err := monitor.ValidateWindow("10:00", "10:00", true); err != nil {
		t.Fatal(err)
	}
}

func TestParseHHMMRequiresUnsignedDigits(t *testing.T) {
	for _, value := range []string{"+1:30", "-1:30", "01:+1", "01:-1", " 1:30", "1:30", "24:00", "00:60"} {
		t.Run(value, func(t *testing.T) {
			if _, _, err := monitor.ParseHHMM(value); err == nil {
				t.Fatalf("accepted malformed time %q", value)
			}
		})
	}
	for _, value := range []string{"00:00", "01:30", "23:59"} {
		if _, _, err := monitor.ParseHHMM(value); err != nil {
			t.Fatalf("rejected valid time %q: %v", value, err)
		}
	}
	if err := monitor.ValidateWindow("+1:30", "01:30", false); err == nil {
		t.Fatal("signed boundary bypassed equal-time validation")
	}
}

func TestPeakScheduleUsesLondonPeriodsAndExclusiveEnds(t *testing.T) {
	location, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		at     time.Time
		active bool
		period string
	}{
		{name: "morning start", at: time.Date(2026, 1, 5, 6, 30, 0, 0, location), active: true, period: "2026-01-05:morning"},
		{name: "morning end", at: time.Date(2026, 1, 5, 9, 30, 0, 0, location), active: false},
		{name: "afternoon start", at: time.Date(2026, 1, 5, 16, 0, 0, 0, location), active: true, period: "2026-01-05:afternoon"},
		{name: "afternoon end", at: time.Date(2026, 1, 5, 19, 0, 0, 0, location), active: false},
		{name: "weekend", at: time.Date(2026, 1, 10, 8, 0, 0, 0, location), active: false},
		{name: "weekday holiday follows weekday", at: time.Date(2026, 12, 25, 8, 0, 0, 0, location), active: true, period: "2026-12-25:morning"},
		{name: "daylight saving", at: time.Date(2026, 7, 6, 7, 0, 0, 0, location), active: true, period: "2026-07-06:morning"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			active, period, expiry := monitor.SchedulePeriod(test.at, monitor.SchedulePeak, "Asia/Tokyo", "", "")
			if active != test.active || period != test.period {
				t.Fatalf("got active=%v period=%q, want active=%v period=%q", active, period, test.active, test.period)
			}
			if active && !expiry.After(test.at) {
				t.Fatalf("expiry %v is not after %v", expiry, test.at)
			}
		})
	}
}

func TestCustomScheduleIncludesEndMinuteAndUsesConfiguredTimezone(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 5, 9, 30, 59, 0, location)
	if !monitor.ScheduleActive(at, monitor.ScheduleCustom, "America/New_York", "07:30", "09:30") {
		t.Fatal("custom end minute was not active")
	}
	if monitor.ScheduleActive(at, monitor.ScheduleCustom, "Europe/London", "07:30", "09:30") {
		t.Fatal("custom schedule ignored configured timezone")
	}
}

func TestCustomScheduleExpiryFollowsClockChanges(t *testing.T) {
	for _, tc := range []struct {
		name, zone, start, end, at, expiry, period string
	}{
		{"new york first hour", "America/New_York", "00:00", "01:30", "2026-11-01T05:15:00Z", "2026-11-01T05:31:00Z", "2026-11-01"},
		{"new york second hour", "America/New_York", "00:00", "01:30", "2026-11-01T06:15:00Z", "2026-11-01T06:31:00Z", "2026-11-01"},
		{"london rollback exits window", "Europe/London", "01:30", "02:30", "2026-10-25T00:45:00Z", "2026-10-25T01:00:00Z", "2026-10-25"},
		{"london second hour", "Europe/London", "01:30", "02:30", "2026-10-25T01:45:00Z", "2026-10-25T02:31:00Z", "2026-10-25"},
		{"transition at end stays active", "Europe/London", "00:30", "01:59", "2026-10-25T00:45:00Z", "2026-10-25T02:00:00Z", "2026-10-25"},
		{"london skipped end", "Europe/London", "00:30", "01:45", "2026-03-29T00:45:00Z", "2026-03-29T01:00:00Z", "2026-03-29"},
		{"london spring continues", "Europe/London", "00:30", "02:30", "2026-03-29T00:45:00Z", "2026-03-29T01:31:00Z", "2026-03-29"},
		{"lord howe first hour", "Australia/Lord_Howe", "01:45", "02:15", "2026-04-04T14:50:00Z", "2026-04-04T15:00:00Z", "2026-04-05"},
		{"lord howe second hour", "Australia/Lord_Howe", "01:45", "02:15", "2026-04-04T15:20:00Z", "2026-04-04T15:46:00Z", "2026-04-05"},
		{"lord howe skipped end", "Australia/Lord_Howe", "01:30", "02:15", "2026-10-03T15:10:00Z", "2026-10-03T15:30:00Z", "2026-10-04"},
		{"overnight repeated hour", "America/New_York", "22:00", "01:30", "2026-11-01T06:15:00Z", "2026-11-01T06:31:00Z", "2026-10-31"},
		{"full day custom resets", "Europe/London", "00:00", "23:59", "2026-10-25T00:45:00Z", "2026-10-26T00:00:00Z", "2026-10-25"},
		{"full day overnight resets", "Europe/London", "12:01", "12:00", "2026-10-25T00:45:00Z", "2026-10-25T12:01:00Z", "2026-10-24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			wantExpiry, err := time.Parse(time.RFC3339, tc.expiry)
			if err != nil {
				t.Fatal(err)
			}
			active, period, expiry := monitor.SchedulePeriod(at.Add(23*time.Second), monitor.ScheduleCustom, tc.zone, tc.start, tc.end)
			if !active || period != tc.period || !expiry.Equal(wantExpiry) || !expiry.After(at) {
				t.Fatalf("active=%v period=%q expiry=%v; want active, %q, %v", active, period, expiry, tc.period, wantExpiry)
			}
		})
	}
}
