package monitor

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	SchedulePeak   = "peak"
	ScheduleAllDay = "all_day"
	ScheduleCustom = "custom"
	LondonTimezone = "Europe/London"
)

type Clock interface{ Now() time.Time }

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func ParseHHMM(value string) (hour, minute int, err error) {
	if len(value) != 5 || value[2] != ':' {
		return 0, 0, errors.New("time must use HH:MM")
	}
	for _, index := range []int{0, 1, 3, 4} {
		if value[index] < '0' || value[index] > '9' {
			return 0, 0, errors.New("time must use HH:MM")
		}
	}
	hour, err = strconv.Atoi(value[:2])
	if err != nil {
		return 0, 0, errors.New("time must use HH:MM")
	}
	minute, err = strconv.Atoi(value[3:])
	if err != nil {
		return 0, 0, errors.New("time must use HH:MM")
	}
	if hour > 23 || minute > 59 {
		return 0, 0, errors.New("time must be between 00:00 and 23:59")
	}
	return hour, minute, nil
}

func ValidateWindow(start, end string, allDay bool) error {
	if allDay {
		return nil
	}
	if _, _, err := ParseHHMM(start); err != nil {
		return fmt.Errorf("window start: %w", err)
	}
	if _, _, err := ParseHHMM(end); err != nil {
		return fmt.Errorf("window end: %w", err)
	}
	if start == end {
		return errors.New("window start and end cannot be equal outside 24/7 mode")
	}
	return nil
}

// ScheduleActive reports whether a profile's line-alert schedule is active.
// Peak hours are always evaluated in London time, independent of profile
// timezone. Custom hours use the configured IANA timezone.
func ScheduleActive(now time.Time, mode, timezone, start, end string) bool {
	active, _, _ := SchedulePeriod(now, mode, timezone, start, end)
	return active
}

// SchedulePeriod returns the active period identity and its delivery expiry.
// The expiry is zero for All day because all-day alerts retain their existing
// delivery rules.
func SchedulePeriod(now time.Time, mode, timezone, start, end string) (bool, string, time.Time) {
	switch mode {
	case SchedulePeak:
		return peakPeriod(now)
	case ScheduleAllDay:
		return true, "all-day", time.Time{}
	case ScheduleCustom:
		return customPeriod(now, timezone, start, end)
	default:
		if start == "" && end == "" {
			return peakPeriod(now)
		}
		return customPeriod(now, timezone, start, end)
	}
}

func peakPeriod(now time.Time) (bool, string, time.Time) {
	location := locationFor(LondonTimezone)
	local := now.In(location)
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday {
		return false, "", time.Time{}
	}
	date := local.Format("2006-01-02")
	morningStart := time.Date(local.Year(), local.Month(), local.Day(), 6, 30, 0, 0, location)
	morningEnd := time.Date(local.Year(), local.Month(), local.Day(), 9, 30, 0, 0, location)
	if !local.Before(morningStart) && local.Before(morningEnd) {
		return true, date + ":morning", morningEnd
	}
	afternoonStart := time.Date(local.Year(), local.Month(), local.Day(), 16, 0, 0, 0, location)
	afternoonEnd := time.Date(local.Year(), local.Month(), local.Day(), 19, 0, 0, 0, location)
	if !local.Before(afternoonStart) && local.Before(afternoonEnd) {
		return true, date + ":afternoon", afternoonEnd
	}
	return false, "", time.Time{}
}

func customPeriod(now time.Time, timezone, start, end string) (bool, string, time.Time) {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return false, "", time.Time{}
	}
	startHour, startMinute, startErr := ParseHHMM(start)
	endHour, endMinute, endErr := ParseHHMM(end)
	if startErr != nil || endErr != nil || start == end {
		return false, "", time.Time{}
	}
	local := now.In(location)
	begin := startHour*60 + startMinute
	finish := endHour*60 + endMinute
	active, period := customPeriodIdentity(local, begin, finish)
	if !active {
		return false, "", time.Time{}
	}
	for cursor := local; ; {
		remaining := finish - (cursor.Hour()*60 + cursor.Minute()) + 1
		if remaining <= 0 {
			remaining += 24 * 60
		}
		elapsed := time.Duration(cursor.Second())*time.Second + time.Duration(cursor.Nanosecond())
		expires := cursor.Add(time.Duration(remaining)*time.Minute - elapsed)
		_, transition := cursor.ZoneBounds()
		if transition.IsZero() || expires.Before(transition) {
			return true, period, expires
		}
		// A clock change can leave the window or skip its nominal end entirely.
		cursor = transition.In(location)
		if active, nextPeriod := customPeriodIdentity(cursor, begin, finish); !active || nextPeriod != period {
			return true, period, cursor
		}
	}
}

func customPeriodIdentity(local time.Time, begin, finish int) (bool, string) {
	current := local.Hour()*60 + local.Minute()
	active := false
	periodDate := local
	if begin < finish {
		active = current >= begin && current <= finish
	} else {
		active = current >= begin || current <= finish
		if current <= finish {
			periodDate = local.AddDate(0, 0, -1)
		}
	}
	if !active {
		return false, ""
	}
	return true, periodDate.Format("2006-01-02")
}
