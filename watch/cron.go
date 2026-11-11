package watch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/luckystrker/ago/events"
)

// ErrInvalidCron reports a cron expression CronWatch cannot parse.
var ErrInvalidCron = errors.New("watch: invalid cron expression")

// maxCronSearchMinutes bounds the next-fire search: one year of minutes. It is
// the Python parser's loop bound (ag2/watch.py, 525960), and the same fallback
// applies when no matching minute is found inside it.
const maxCronSearchMinutes = 525960

// dowNames maps the three-letter weekday names Python's parser accepts. Values
// are 0=Sunday..6=Saturday, so they line up with time.Weekday().
var dowNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// CronOption configures CronWatch.
type CronOption func(*cronWatch)

// WithCronNow replaces the clock used to compute the next fire time. It exists
// so a host (or a test) can drive the schedule deterministically; nil keeps
// time.Now.
func WithCronNow(now func() time.Time) CronOption {
	return func(w *cronWatch) {
		if now != nil {
			w.now = now
		}
	}
}

// CronWatch fires on a standard 5-field cron expression:
// "minute hour day-of-month month day-of-week".
//
// Supported field syntax (Python parity: ag2/watch.py's own parser, no external
// dependency): "*", "a", "a-b", "*/step", "start/step", and comma-separated
// lists of those. The day-of-week field also accepts SUN..SAT; numeric values
// 0-7 are accepted with 7 aliasing Sunday. As in Python, day-of-month and
// day-of-week are ANDed when both are restricted (not the OR some cron
// implementations use).
//
// A schedule that needs more than a year of searching has no fire time inside
// the horizon and falls back to one hour after now (Python parity). One Wait is
// one armed fire: the watch computes the next matching minute after the clock
// reading and sleeps until it, reporting the tick as (nil, nil).
//
// AGO-DEVIATION: out-of-range or malformed fields are rejected at parse time
// rather than accepted and silently never matched (Python only fails when the
// search finds nothing). AGO-DEVIATION: the search advances on the absolute
// clock, so a daylight-saving jump is handled by time.Add; Python's naive
// datetime arithmetic ignores it.
func CronWatch(expression string, opts ...CronOption) (Watch, error) {
	schedule, err := parseCron(expression)
	if err != nil {
		return nil, err
	}
	w := &cronWatch{schedule: schedule, now: time.Now, after: time.After}
	for _, opt := range opts {
		opt(w)
	}
	return w, nil
}

type cronWatch struct {
	schedule cronSchedule
	now      func() time.Time
	// after is the timer used for the wait; tests replace it to avoid sleeping
	// the scheduled delay.
	after func(time.Duration) <-chan time.Time
}

func (w *cronWatch) Wait(ctx context.Context) (events.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := w.now()
	next := w.schedule.next(now)
	if d := next.Sub(now); d > 0 {
		select {
		case <-w.after(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

// cronSchedule is a parsed expression: one set per field.
type cronSchedule struct {
	minute, hour, dayOfMonth, month, dayOfWeek map[int]bool
}

// next returns the first minute strictly after now (truncated to the minute)
// that matches every field, or one hour after now when the search horizon is
// exhausted.
func (s cronSchedule) next(now time.Time) time.Time {
	candidate := now.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < maxCronSearchMinutes; i++ {
		if s.minute[candidate.Minute()] &&
			s.hour[candidate.Hour()] &&
			s.dayOfMonth[candidate.Day()] &&
			s.month[int(candidate.Month())] &&
			s.dayOfWeek[int(candidate.Weekday())] {
			return candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	return now.Add(time.Hour)
}

// parseCron parses the five whitespace-separated fields.
func parseCron(expression string) (cronSchedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return cronSchedule{}, fmt.Errorf("%w: %q (need 5 fields)", ErrInvalidCron, expression)
	}
	minute, err := parseCronField(fields[0], 0, 59, false)
	if err != nil {
		return cronSchedule{}, fmt.Errorf("%w: minute field %q: %v", ErrInvalidCron, fields[0], err)
	}
	hour, err := parseCronField(fields[1], 0, 23, false)
	if err != nil {
		return cronSchedule{}, fmt.Errorf("%w: hour field %q: %v", ErrInvalidCron, fields[1], err)
	}
	dayOfMonth, err := parseCronField(fields[2], 1, 31, false)
	if err != nil {
		return cronSchedule{}, fmt.Errorf("%w: day-of-month field %q: %v", ErrInvalidCron, fields[2], err)
	}
	month, err := parseCronField(fields[3], 1, 12, false)
	if err != nil {
		return cronSchedule{}, fmt.Errorf("%w: month field %q: %v", ErrInvalidCron, fields[3], err)
	}
	dow, err := parseCronField(fields[4], 0, 7, true)
	if err != nil {
		return cronSchedule{}, fmt.Errorf("%w: day-of-week field %q: %v", ErrInvalidCron, fields[4], err)
	}
	// 7 aliases Sunday; the weekday set stores time.Weekday() values.
	dayOfWeek := make(map[int]bool, len(dow))
	for v := range dow {
		dayOfWeek[v%7] = true
	}
	return cronSchedule{minute: minute, hour: hour, dayOfMonth: dayOfMonth, month: month, dayOfWeek: dayOfWeek}, nil
}

// parseCronField expands one field into its value set. names enables the
// day-of-week names. Every form Python's parser supports is accepted, and the
// union of comma-separated parts is returned.
func parseCronField(spec string, min, max int, names bool) (map[int]bool, error) {
	if spec == "" {
		return nil, errors.New("empty field")
	}
	out := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		switch {
		case part == "":
			return nil, errors.New("empty list element")
		case part == "*":
			for v := min; v <= max; v++ {
				out[v] = true
			}
		case strings.Contains(part, "/"):
			base, stepText, ok := strings.Cut(part, "/")
			if !ok || strings.Contains(stepText, "/") {
				return nil, fmt.Errorf("malformed step %q", part)
			}
			step, err := strconv.Atoi(stepText)
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("invalid step %q", part)
			}
			start := min
			if base != "*" {
				start, err = strconv.Atoi(base)
				if err != nil {
					return nil, fmt.Errorf("invalid step base %q", part)
				}
			}
			if start < min || start > max {
				return nil, fmt.Errorf("value %d out of range %d-%d", start, min, max)
			}
			for v := start; v <= max; v += step {
				out[v] = true
			}
		case strings.Contains(part, "-"):
			loText, hiText, ok := strings.Cut(part, "-")
			if !ok {
				return nil, fmt.Errorf("malformed range %q", part)
			}
			lo, err := strconv.Atoi(loText)
			if err != nil {
				return nil, fmt.Errorf("invalid range start %q", part)
			}
			hi, err := strconv.Atoi(hiText)
			if err != nil {
				return nil, fmt.Errorf("invalid range end %q", part)
			}
			if lo > hi {
				return nil, fmt.Errorf("descending range %q", part)
			}
			if lo < min || hi > max {
				return nil, fmt.Errorf("range %d-%d out of range %d-%d", lo, hi, min, max)
			}
			for v := lo; v <= hi; v++ {
				out[v] = true
			}
		default:
			if names {
				if v, ok := dowNames[strings.ToUpper(part)]; ok {
					out[v] = true
					continue
				}
			}
			v, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid value %q", part)
			}
			if v < min || v > max {
				return nil, fmt.Errorf("value %d out of range %d-%d", v, min, max)
			}
			out[v] = true
		}
	}
	return out, nil
}
