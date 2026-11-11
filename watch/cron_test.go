package watch

import (
	"errors"
	"testing"
	"time"
)

// utc builds a fixed time for the schedule tests.
func utc(year int, month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(year, month, day, hour, minute, second, 0, time.UTC)
}

// TestCronNextFire pins the next-fire search across minute, hour, day, week,
// month and year boundaries. Every expression in the table is either one of the
// forms the parser documents or a boundary case built from those forms.
func TestCronNextFire(t *testing.T) {
	tests := []struct {
		name string
		expr string
		now  time.Time
		want time.Time
	}{
		{
			name: "every minute crosses the hour boundary",
			expr: "* * * * *",
			now:  utc(2026, time.November, 10, 10, 59, 30),
			want: utc(2026, time.November, 10, 11, 0, 0),
		},
		{
			name: "every minute exactly on a minute boundary waits a full minute",
			expr: "* * * * *",
			now:  utc(2026, time.November, 10, 10, 59, 0),
			want: utc(2026, time.November, 10, 11, 0, 0),
		},
		{
			name: "step expression",
			expr: "*/5 * * * *",
			now:  utc(2026, time.November, 10, 10, 7, 30),
			want: utc(2026, time.November, 10, 10, 10, 0),
		},
		{
			name: "step expression starts at the next aligned minute",
			expr: "*/15 * * * *",
			now:  utc(2026, time.November, 10, 10, 45, 1),
			want: utc(2026, time.November, 10, 11, 0, 0),
		},
		{
			name: "range upper bound is inclusive",
			expr: "0 9-10 * * *",
			now:  utc(2026, time.November, 10, 9, 30, 0),
			want: utc(2026, time.November, 10, 10, 0, 0),
		},
		{
			name: "range ending on the field maximum is valid",
			expr: "0 8-23 * * *",
			now:  utc(2026, time.November, 10, 22, 30, 0),
			want: utc(2026, time.November, 10, 23, 0, 0),
		},
		{
			name: "range starting on the field minimum is valid",
			expr: "0 0 1-2 * *",
			now:  utc(2026, time.November, 10, 12, 0, 0),
			want: utc(2026, time.December, 1, 0, 0, 0),
		},
		{
			name: "single-value range a-a is a valid one-value range",
			expr: "0 5-5 * * *",
			now:  utc(2026, time.November, 10, 5, 30, 0),
			want: utc(2026, time.November, 11, 5, 0, 0),
		},
		{
			name: "step keeps the last value when it lands on the field maximum",
			expr: "19/20 * * * *",
			now:  utc(2026, time.November, 10, 19, 40, 0),
			want: utc(2026, time.November, 10, 19, 59, 0),
		},
		{
			name: "step base equal to the field maximum is accepted",
			expr: "59/10 * * * *",
			now:  utc(2026, time.November, 10, 10, 0, 0),
			want: utc(2026, time.November, 10, 10, 59, 0),
		},
		{
			name: "step base equal to the field minimum is accepted",
			expr: "0/20 * * * *",
			now:  utc(2026, time.November, 10, 10, 5, 0),
			want: utc(2026, time.November, 10, 10, 20, 0),
		},
		{
			name: "start/step expression",
			expr: "10/20 * * * *",
			now:  utc(2026, time.November, 10, 10, 31, 0),
			want: utc(2026, time.November, 10, 10, 50, 0),
		},
		{
			name: "list and hour range crosses to the next day",
			expr: "15,45 8-18 * * *",
			now:  utc(2026, time.November, 10, 18, 50, 0),
			want: utc(2026, time.November, 11, 8, 15, 0),
		},
		{
			name: "midnight crosses the day boundary",
			expr: "0 0 * * *",
			now:  utc(2026, time.November, 10, 23, 59, 30),
			want: utc(2026, time.November, 11, 0, 0, 0),
		},
		{
			name: "weekday name",
			expr: "0 9 * * MON",
			now:  utc(2026, time.November, 10, 10, 0, 0), // Tuesday
			want: utc(2026, time.November, 16, 9, 0, 0),
		},
		{
			name: "weekday name on the same day before the fire time",
			expr: "0 9 * * MON",
			now:  utc(2026, time.November, 16, 8, 59, 30),
			want: utc(2026, time.November, 16, 9, 0, 0),
		},
		{
			name: "numeric Sunday alias 7",
			expr: "0 0 * * 7",
			now:  utc(2026, time.November, 14, 12, 0, 0), // Saturday
			want: utc(2026, time.November, 15, 0, 0, 0),
		},
		{
			name: "Sunday name matches the numeric alias",
			expr: "0 0 * * SUN",
			now:  utc(2026, time.November, 14, 12, 0, 0),
			want: utc(2026, time.November, 15, 0, 0, 0),
		},
		{
			name: "month boundary",
			expr: "0 0 1 * *",
			now:  utc(2026, time.November, 10, 12, 0, 0),
			want: utc(2026, time.December, 1, 0, 0, 0),
		},
		{
			name: "year boundary",
			expr: "0 0 1 1 *",
			now:  utc(2026, time.November, 10, 12, 0, 0),
			want: utc(2027, time.January, 1, 0, 0, 0),
		},
		{
			name: "day-of-month and weekday are ANDed",
			expr: "0 12 10 * TUE",
			now:  utc(2026, time.November, 10, 13, 0, 0), // Tuesday the 10th, after noon
			// OR semantics would fire next Tuesday (Nov 17); AND waits for the
			// next Tuesday that is also the 10th, Aug 10 2027.
			want: utc(2027, time.August, 10, 12, 0, 0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := CronWatch(tt.expr)
			if err != nil {
				t.Fatalf("CronWatch(%q): %v", tt.expr, err)
			}
			got := w.(*cronWatch).schedule.next(tt.now)
			if !got.Equal(tt.want) {
				t.Fatalf("next(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

// TestCronSearchHorizon pins the Python fallback: an impossible schedule has no
// fire time inside one year and reports one hour after now.
func TestCronSearchHorizon(t *testing.T) {
	tests := []struct {
		name string
		expr string
		now  time.Time
	}{
		{name: "February 29 outside the one-year horizon", expr: "0 0 29 2 *", now: utc(2026, time.March, 1, 0, 0, 0)},
		{name: "April 31 never exists", expr: "0 0 31 4 *", now: utc(2026, time.April, 1, 0, 0, 0)},
		{name: "February 30 never exists", expr: "0 0 30 2 *", now: utc(2026, time.February, 1, 0, 0, 0)},
		{
			// Exactly 525960 minutes (365.25 days) after the first candidate:
			// the last minute inside Python's range(525960) horizon is
			// 2027-11-10 16:00, so 16:01 is unreachable and the fallback wins.
			name: "match exactly one minute past the horizon falls back",
			expr: "1 16 10 11 WED",
			now:  utc(2026, time.November, 10, 10, 0, 30),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := CronWatch(tt.expr)
			if err != nil {
				t.Fatalf("CronWatch(%q): %v", tt.expr, err)
			}
			got := w.(*cronWatch).schedule.next(tt.now)
			if want := tt.now.Add(time.Hour); !got.Equal(want) {
				t.Fatalf("next(%v) = %v, want the one-hour fallback %v", tt.now, got, want)
			}
		})
	}
}

// TestCronRejectsMalformedExpressions pins the declared parse-time deviation:
// Python accepts out-of-range values and simply never matches them, while AGo
// rejects them when the watch is built.
func TestCronRejectsMalformedExpressions(t *testing.T) {
	tests := []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * 32 * *",
		"* * * 13 *",
		"* * * * 8",
		"*/0 * * * *",
		"60/5 * * * *",
		"-1/5 * * * *",
		"1-5/2 * * * *",
		"1/2/3 * * * *",
		"5-1 * * * *",
		"5-3 * * * *",
		"0 0 0-5 * *",
		"0 0 1-32 * *",
		"MON * * * *",
		"abc * * * *",
		"1,,2 * * * *",
	}
	for _, expr := range tests {
		t.Run(expr, func(t *testing.T) {
			if _, err := CronWatch(expr); !errors.Is(err, ErrInvalidCron) {
				t.Fatalf("CronWatch(%q) error = %v, want ErrInvalidCron", expr, err)
			}
		})
	}
}

// TestCronWaitUsesTheClock pins the wait path on a fake clock: the delay is
// next-fire minus now, and an elapsed schedule fires immediately.
func TestCronWaitUsesTheClock(t *testing.T) {
	tests := []struct {
		name      string
		expr      string
		now       time.Time
		wantDelay time.Duration
	}{
		{
			name:      "waits until the next minute",
			expr:      "* * * * *",
			now:       utc(2026, time.November, 10, 10, 59, 59),
			wantDelay: time.Second,
		},
		{
			name:      "waits until the next day",
			expr:      "0 0 * * *",
			now:       utc(2026, time.November, 10, 23, 59, 59),
			wantDelay: time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := CronWatch(tt.expr, WithCronNow(func() time.Time { return tt.now }))
			if err != nil {
				t.Fatalf("CronWatch: %v", err)
			}
			cw := w.(*cronWatch)
			var gotDelay time.Duration
			fired := make(chan time.Time)
			cw.after = func(d time.Duration) <-chan time.Time {
				gotDelay = d
				close(fired)
				return fired
			}
			ev, err := cw.Wait(waitCtx(t))
			if err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if ev != nil {
				t.Fatalf("event = %v, want nil tick", ev)
			}
			if gotDelay != tt.wantDelay {
				t.Fatalf("delay = %v, want %v", gotDelay, tt.wantDelay)
			}
		})
	}
}
