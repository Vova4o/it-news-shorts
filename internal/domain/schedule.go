package domain

import "time"

const moscowOffset = 3 * time.Hour

// Moscow returns the Europe/Moscow location. Moscow has been on a fixed UTC+3
// offset since 2014, so a fixed zone is exact and does not depend on tzdata
// being installed in the runtime image.
func Moscow() *time.Location {
	return time.FixedZone("MSK", int(moscowOffset/time.Second))
}

// DailySchedule computes the next daily run and its news window.
type DailySchedule struct {
	hour   int
	minute int
	loc    *time.Location
}

// NewDailySchedule validates the schedule parameters.
func NewDailySchedule(hour, minute int, loc *time.Location) (DailySchedule, error) {
	if loc == nil {
		return DailySchedule{}, invalid("schedule.location")
	}
	if hour < 0 || hour > 23 {
		return DailySchedule{}, invalid("schedule.hour")
	}
	if minute < 0 || minute > 59 {
		return DailySchedule{}, invalid("schedule.minute")
	}
	return DailySchedule{hour: hour, minute: minute, loc: loc}, nil
}

// MoscowSchedule is the production schedule: every day at 09:00 MSK.
func MoscowSchedule() DailySchedule {
	return DailySchedule{hour: DailyRunHourMSK, minute: 0, loc: Moscow()}
}

// Location returns the schedule time zone.
func (s DailySchedule) Location() *time.Location { return s.loc }

// Hour returns the run hour in the schedule time zone.
func (s DailySchedule) Hour() int { return s.hour }

// Minute returns the run minute in the schedule time zone.
func (s DailySchedule) Minute() int { return s.minute }

// NextRun returns the first run strictly after the given moment.
func (s DailySchedule) NextRun(after time.Time) time.Time {
	local := after.In(s.loc)
	candidate := time.Date(local.Year(), local.Month(), local.Day(), s.hour, s.minute, 0, 0, s.loc)
	if !candidate.After(local) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

// WindowFor returns the 24 hour news window that ends at the run moment.
func (s DailySchedule) WindowFor(runAt time.Time) TimeWindow {
	return TimeWindow{Start: runAt.Add(-NewsWindow), End: runAt}
}
