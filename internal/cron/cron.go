// Package cron parses maintenance-window schedules and evaluates
// window openness (PLAN.md §3.3).
//
// Accepted set — Vixie cron as documented in FreeBSD crontab(5), plus
// the controller's `@every <duration>` extension, nothing else:
// 5-field cron (names allowed), the Vixie `@` schedules,
// `@every <duration>`, and the Vixie numeric `@<seconds>` form (alias
// for `@every <N>s`). Rejected: `@reboot`, 6-field expressions,
// weekday `@` aliases, unknown `@` names, empty strings, out-of-range
// values, and `?`.
//
// Evaluation is stateless: openness is computed from the clock alone,
// so every pod and leader computes the same answer across handovers.
// `@every` occurrences are anchored at midnight UTC (not at process
// start) for exactly that reason.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is one parsed recurrence. The zero value is invalid;
// obtain values with Parse.
type Schedule struct {
	spec string
	// every != 0 for the @every / @<seconds> / @every_second forms:
	// occurrences are midnight-UTC + k*every.
	every time.Duration
	// 5-field form: sorted ascending value lists.
	minutes, hours, dom, months, dow []int
	// domStar/dowStar record a literal "*" field (PLAN.md §3.3: only a
	// bare "*" counts as unrestricted — "*/2" is restricted).
	domStar, dowStar bool
}

// String returns the original spec text.
func (s Schedule) String() string { return s.spec }

// Parse parses one schedule string.
func Parse(spec string) (Schedule, error) {
	if strings.HasPrefix(spec, "@") {
		return parseAt(spec)
	}
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron: want 5 fields, got %d in %q", len(fields), spec)
	}
	minutes, err := parseField(fields[0], 0, 59, nil)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: minute: %w", err)
	}
	hours, err := parseField(fields[1], 0, 23, nil)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: hour: %w", err)
	}
	dom, err := parseField(fields[2], 1, 31, nil)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: day of month: %w", err)
	}
	months, err := parseField(fields[3], 1, 12, monthNames)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: month: %w", err)
	}
	dow, err := parseField(fields[4], 0, 7, dowNames)
	if err != nil {
		return Schedule{}, fmt.Errorf("cron: day of week: %w", err)
	}
	// 7 is Sunday too (Vixie); normalize to 0.
	norm := dow[:0]
	seenSun := false
	for _, d := range dow {
		if d == 7 {
			if !seenSun {
				norm = append(norm, 0)
				seenSun = true
			}
			continue
		}
		norm = append(norm, d)
	}
	return Schedule{
		spec:    spec,
		minutes: minutes,
		hours:   hours,
		dom:     dom,
		months:  months,
		dow:     norm,
		domStar: fields[2] == "*",
		dowStar: fields[4] == "*",
	}, nil
}

// monthNames/dowNames map the Vixie 3-letter abbreviations
// (case-insensitive) to values. Only these abbreviations are accepted.
var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// parseAt parses the "@" forms.
func parseAt(spec string) (Schedule, error) {
	fields := strings.Fields(spec)
	switch fields[0] {
	case "@hourly":
		return mustFields(spec, fields, "0 * * * *")
	case "@daily", "@midnight":
		return mustFields(spec, fields, "0 0 * * *")
	case "@weekly":
		return mustFields(spec, fields, "0 0 * * 0")
	case "@monthly":
		return mustFields(spec, fields, "0 0 1 * *")
	case "@yearly", "@annually":
		return mustFields(spec, fields, "0 0 1 1 *")
	case "@every_minute":
		return mustFields(spec, fields, "* * * * *")
	case "@every_second":
		if len(fields) != 1 {
			return Schedule{}, fmt.Errorf("cron: %q takes no argument", fields[0])
		}
		return Schedule{spec: spec, every: time.Second}, nil
	case "@every":
		if len(fields) != 2 {
			return Schedule{}, fmt.Errorf("cron: want \"@every <duration>\", got %q", spec)
		}
		d, err := time.ParseDuration(fields[1])
		if err != nil || d <= 0 {
			return Schedule{}, fmt.Errorf("cron: bad @every duration %q", fields[1])
		}
		return Schedule{spec: spec, every: d}, nil
	}
	// Vixie numeric form: @<seconds>.
	if len(fields) == 1 {
		if n, err := strconv.Atoi(strings.TrimPrefix(fields[0], "@")); err == nil {
			if n <= 0 {
				return Schedule{}, fmt.Errorf("cron: bad @seconds value %q", spec)
			}
			return Schedule{spec: spec, every: time.Duration(n) * time.Second}, nil
		}
	}
	return Schedule{}, fmt.Errorf("cron: unknown schedule %q", spec)
}

// mustFields parses a fixed 5-field equivalent of a named schedule;
// the literal must stand alone (no trailing argument).
func mustFields(spec string, fields []string, equiv string) (Schedule, error) {
	if len(fields) != 1 {
		return Schedule{}, fmt.Errorf("cron: %q takes no argument", fields[0])
	}
	s, err := Parse(equiv)
	if err != nil {
		panic("cron: bad built-in equivalent " + equiv)
	}
	s.spec = spec
	return s, nil
}

// parseField parses one cron field: "*", value, list, range, step
// ("*/n", "a-b/n"); lists and ranges may mix. names maps
// abbreviations to values (nil = numeric only).
func parseField(field string, min, max int, names map[string]int) ([]int, error) {
	if strings.Contains(field, "?") {
		return nil, fmt.Errorf("%q: '?' is not accepted, use '*'", field)
	}
	present := make(map[int]bool)
	for _, item := range strings.Split(field, ",") {
		if item == "" {
			return nil, fmt.Errorf("%q: empty list element", field)
		}
		base, step := item, 1
		if i := strings.Index(item, "/"); i >= 0 {
			base, step = item[:i], 0
			n, err := strconv.Atoi(item[i+1:])
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%q: bad step", item)
			}
			step = n
		}
		lo, hi := 0, 0
		switch {
		case base == "*":
			lo, hi = min, max
		case strings.Contains(base, "-"):
			parts := strings.SplitN(base, "-", 2)
			var err error
			if lo, err = parseValue(parts[0], min, max, names); err != nil {
				return nil, fmt.Errorf("%q: %w", item, err)
			}
			if hi, err = parseValue(parts[1], min, max, names); err != nil {
				return nil, fmt.Errorf("%q: %w", item, err)
			}
			if lo > hi {
				return nil, fmt.Errorf("%q: range start after end", item)
			}
		default:
			// A bare "a/n" step base is not Vixie — only "*" and
			// "a-b" take steps.
			if strings.Contains(item, "/") {
				return nil, fmt.Errorf("%q: steps need '*' or a range", item)
			}
			var err error
			if lo, err = parseValue(base, min, max, names); err != nil {
				return nil, fmt.Errorf("%q: %w", item, err)
			}
			hi = lo
		}
		for v := lo; v <= hi; v += step {
			present[v] = true
		}
	}
	if len(present) == 0 {
		return nil, fmt.Errorf("%q: empty field", field)
	}
	out := make([]int, 0, len(present))
	for v := range present {
		out = append(out, v)
	}
	// Insertion order above is ascending except for mixed lists
	// ("8,1-3"); sort for the day scan. Sizes are tiny.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// parseValue parses one numeric or named field value.
func parseValue(raw string, min, max int, names map[string]int) (int, error) {
	if names != nil {
		if v, ok := names[strings.ToLower(raw)]; ok {
			return v, nil
		}
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%q out of range %d-%d", raw, min, max)
	}
	return n, nil
}

// lastOccurrenceAtOrBefore returns the latest occurrence at or before
// t. ok == false means the schedule has no occurrence at or before t
// within the bounded search horizon (e.g. February 30, which parses
// fine and never occurs); a schedule with no occurrence makes its
// window closed. A zero time.Time never stands in for an occurrence.
func (s Schedule) lastOccurrenceAtOrBefore(t time.Time) (time.Time, bool) {
	t = t.UTC()
	if s.every != 0 {
		midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		k := t.Sub(midnight) / s.every
		return midnight.Add(time.Duration(k) * s.every), true
	}
	lim := t.Truncate(time.Minute)
	horizon := 366
	// Feb-29-only schedules need a 4-year horizon (PLAN.md §3.3).
	if isOnlyFeb29(s.months, s.dom) {
		horizon = 4*366 + 1
	}
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	for d := 0; d <= horizon; d++ {
		dd := day.AddDate(0, 0, -d)
		if !contains(s.months, int(dd.Month())) {
			continue
		}
		if !s.dayMatches(dd) {
			continue
		}
		maxH, maxM := 23, 59
		if d == 0 {
			maxH, maxM = lim.Hour(), lim.Minute()
		}
		for i := len(s.hours) - 1; i >= 0; i-- {
			h := s.hours[i]
			if h > maxH {
				continue
			}
			top := 59
			if h == maxH {
				top = maxM
			}
			for j := len(s.minutes) - 1; j >= 0; j-- {
				if s.minutes[j] <= top {
					return time.Date(dd.Year(), dd.Month(), dd.Day(), h, s.minutes[j], 0, 0, time.UTC), true
				}
			}
		}
	}
	return time.Time{}, false
}

// isOnlyFeb29 reports whether the month/dom lists can only match
// February 29 (the one sparser-than-yearly case).
func isOnlyFeb29(months, dom []int) bool {
	return len(months) == 1 && months[0] == 2 && len(dom) == 1 && dom[0] == 29
}

// dayMatches applies the dom/dow rule (Vixie/K8s): when both fields
// are restricted (neither is a bare "*"), a day matches if either
// matches; a bare "*" side defers to the other.
func (s Schedule) dayMatches(day time.Time) bool {
	domMatch := contains(s.dom, day.Day())
	dowMatch := contains(s.dow, int(day.Weekday()))
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowMatch
	case s.dowStar:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

func contains(vs []int, v int) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

// WindowsOpen reports whether any schedule has an occurrence O with
// O <= now < O+grace. A zero grace or no schedules is always closed.
// now is the cycle's single sampled clock value: every candidate in
// the cycle sees the same openness (PLAN.md §3.5).
func WindowsOpen(specs []Schedule, grace time.Duration, now time.Time) bool {
	if grace <= 0 || len(specs) == 0 {
		return false
	}
	now = now.UTC()
	for _, s := range specs {
		if o, ok := s.lastOccurrenceAtOrBefore(now); ok {
			if !o.After(now) && now.Sub(o) < grace {
				return true
			}
		}
	}
	return false
}
