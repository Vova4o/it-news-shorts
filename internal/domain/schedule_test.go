package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

func TestMoscowScheduleNextRun(t *testing.T) {
	moscow := domain.Moscow()
	schedule := domain.MoscowSchedule()
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			"minute before the run",
			time.Date(2026, time.March, 1, 8, 59, 59, 0, moscow),
			time.Date(2026, time.March, 1, 9, 0, 0, 0, moscow),
		},
		{
			"exactly at the run",
			time.Date(2026, time.March, 1, 9, 0, 0, 0, moscow),
			time.Date(2026, time.March, 2, 9, 0, 0, 0, moscow),
		},
		{
			"later the same day",
			time.Date(2026, time.March, 1, 12, 0, 0, 0, moscow),
			time.Date(2026, time.March, 2, 9, 0, 0, 0, moscow),
		},
		{
			"utc input in the evening",
			time.Date(2026, time.November, 12, 20, 12, 0, 0, time.UTC),
			time.Date(2026, time.November, 13, 9, 0, 0, 0, moscow),
		},
		{
			"month end",
			time.Date(2026, time.November, 30, 10, 0, 0, 0, moscow),
			time.Date(2026, time.December, 1, 9, 0, 0, 0, moscow),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := schedule.NextRun(tt.now)
			if !got.Equal(tt.want) {
				t.Fatalf("NextRun(%s) = %s, want %s", tt.now.Format(time.RFC3339), got.Format(time.RFC3339), tt.want.Format(time.RFC3339))
			}
			if _, offset := got.Zone(); offset != 3*3600 {
				t.Fatalf("zone offset = %d, want 10800 (MSK)", offset)
			}
			if got.Hour() != 9 || got.Minute() != 0 {
				t.Fatalf("run time = %02d:%02d, want 09:00", got.Hour(), got.Minute())
			}
		})
	}
}

func TestWindowForCoversPrevious24Hours(t *testing.T) {
	schedule := domain.MoscowSchedule()
	runAt := time.Date(2026, time.November, 13, 9, 0, 0, 0, domain.Moscow())
	window := schedule.WindowFor(runAt)

	if err := window.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got, want := window.Duration(), 24*time.Hour; got != want {
		t.Fatalf("Duration() = %v, want %v", got, want)
	}
	if !window.Contains(window.Start) {
		t.Fatal("window must include its start instant")
	}
	if window.Contains(window.End) {
		t.Fatal("window must exclude its end instant")
	}
	if window.Contains(window.Start.Add(-time.Nanosecond)) {
		t.Fatal("window must exclude instants before its start")
	}
	if !window.Contains(window.End.Add(-time.Nanosecond)) {
		t.Fatal("window must include the last nanosecond before its end")
	}
	if !window.Contains(runAt.Add(-23 * time.Hour)) {
		t.Fatal("window must include news from 23 hours ago")
	}
	if window.Contains(runAt.Add(-25 * time.Hour)) {
		t.Fatal("window must exclude news older than 24 hours")
	}
}

func TestTimeWindowRejectsWrongLength(t *testing.T) {
	tests := []struct {
		name  string
		start time.Time
		end   time.Time
	}{
		{"too short", baseTime, baseTime.Add(23 * time.Hour)},
		{"too long", baseTime, baseTime.Add(25 * time.Hour)},
		{"inverted", baseTime, baseTime.Add(-24 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := domain.NewTimeWindow(tt.start, tt.end); !errors.Is(err, domain.ErrInvalidWindow) && !errors.Is(err, domain.ErrInvalidValue) {
				t.Fatalf("error = %v, want a window error", err)
			}
		})
	}
	if _, err := domain.NewTimeWindow(baseTime.Add(-24*time.Hour), baseTime); err != nil {
		t.Fatalf("NewTimeWindow(valid) error = %v", err)
	}
}

func TestNewDailyScheduleValidation(t *testing.T) {
	tests := []struct {
		name   string
		hour   int
		minute int
		loc    *time.Location
	}{
		{"hour too large", 24, 0, time.UTC},
		{"negative hour", -1, 0, time.UTC},
		{"minute too large", 9, 60, time.UTC},
		{"negative minute", 9, -1, time.UTC},
		{"missing location", 9, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := domain.NewDailySchedule(tt.hour, tt.minute, tt.loc); !errors.Is(err, domain.ErrInvalidValue) {
				t.Fatalf("error = %v, want %v", err, domain.ErrInvalidValue)
			}
		})
	}
	schedule, err := domain.NewDailySchedule(6, 30, time.UTC)
	if err != nil {
		t.Fatalf("NewDailySchedule(valid) error = %v", err)
	}
	if schedule.Hour() != 6 || schedule.Minute() != 30 {
		t.Fatalf("schedule = %02d:%02d, want 06:30", schedule.Hour(), schedule.Minute())
	}
}
