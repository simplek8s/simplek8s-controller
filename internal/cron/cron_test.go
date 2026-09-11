package cron

import (
	"testing"
	"time"
)

func utc(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

func mustParse(t *testing.T, spec string) Schedule {
	t.Helper()
	s, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q) = %v", spec, err)
	}
	return s
}

func TestParseValid(t *testing.T) {
	for _, spec := range []string{
		"* * * * *",
		"0 7 * * 3",
		"*/10 * * * *",
		"0 0 1-15 * 1-5",
		"1-3,7-9 * * * *",  // lists and ranges mix (Vixie)
		"0-23/2 * * * *",   // stepped range
		"5 4 * * sun",      // dow name
		"0 0 1 jan *",      // month name
		"0 22 * * MON-FRI", // names case-insensitive, in ranges
		"0 0 * JAN,apr *",  // names case-insensitive, in lists
		"0 0 * * 7",        // 7 = Sunday
		"@hourly", "@daily", "@midnight", "@weekly", "@monthly",
		"@yearly", "@annually", "@every_minute", "@every_second",
		"@every 30m", "@every 2h", "@every 12h",
		"@300", "@1", // Vixie numeric form
	} {
		if _, err := Parse(spec); err != nil {
			t.Errorf("Parse(%q) = %v, want nil", spec, err)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	for _, spec := range []string{
		"",
		"   ",
		"*/10 * * *",         // 4 fields
		"* * * * * *",        // 6 fields
		"@monday", "@sunday", // Quartz-isms, not Vixie
		"@reboot",                       // startup-anchored, no occurrence set
		"@nope",                         // unknown @ name
		"@",                             // bare @
		"@0", "@every 0s", "@every -5m", // non-positive periods
		"@every",          // missing duration
		"@every 1d",       // Go ParseDuration has no days
		"@every 2h extra", // trailing argument
		"@daily extra",    // named takes no argument
		"0 0 * * ?",       // ? is not Vixie
		"61 * * * *",      // minute out of range
		"* 25 * * *",      // hour out of range
		"0 0 0 * *",       // dom out of range
		"0 0 32 * *",      // dom out of range
		"0 0 * 0 *",       // month out of range
		"0 0 * 13 *",      // month out of range
		"0 0 * * 8",       // dow out of range
		"5/15 * * * *",    // bare-value step base is not Vixie
		"8-5 * * * *",     // range start after end
		"*/0 * * * *",     // zero step
		"1,,2 * * * *",    // empty list element
		"0 0 * foo *",     // unknown month name
		"* * * * monday",  // full weekday name is not accepted
		"0 0 1 january *", // full month name is not accepted
	} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) = nil, want error", spec)
		}
	}
	// "mon" alone in dow IS valid — the guard above keeps the table
	// honest about which entries are meant to fail.
	if _, err := Parse("0 0 * * mon"); err != nil {
		t.Errorf("Parse(%q) = %v, want nil", "0 0 * * mon", err)
	}
}

func TestLastOccurrenceBasic(t *testing.T) {
	cases := []struct {
		spec string
		at   time.Time
		want time.Time
	}{
		{"0 0 * * *", utc(2026, 9, 11, 10, 0, 0), utc(2026, 9, 11, 0, 0, 0)},
		{"0 0 * * *", utc(2026, 9, 11, 0, 0, 0), utc(2026, 9, 11, 0, 0, 0)},
		{"*/10 * * * *", utc(2026, 9, 11, 10, 7, 0), utc(2026, 9, 11, 10, 0, 0)},
		{"0 10 * * *", utc(2026, 9, 11, 10, 0, 30), utc(2026, 9, 11, 10, 0, 0)}, // sub-minute truncated
		{"0 10 * * *", utc(2026, 9, 11, 9, 59, 59), utc(2026, 9, 10, 10, 0, 0)},
		{"0 22 * * mon-fri", utc(2026, 9, 11, 23, 0, 0), utc(2026, 9, 11, 22, 0, 0)}, // Friday
		{"0 22 * * mon-fri", utc(2026, 9, 12, 1, 0, 0), utc(2026, 9, 11, 22, 0, 0)},  // Saturday -> Friday
		{"0 0 * * 7", utc(2026, 9, 13, 5, 0, 0), utc(2026, 9, 13, 0, 0, 0)},          // Sunday via 7
		{"0 0 1 JAN *", utc(2026, 9, 11, 0, 0, 0), utc(2026, 1, 1, 0, 0, 0)},
		{"@every 2h", utc(2026, 9, 11, 10, 30, 0), utc(2026, 9, 11, 10, 0, 0)},
		{"@every 2h", utc(2026, 9, 11, 0, 0, 0), utc(2026, 9, 11, 0, 0, 0)},
		{"@300", utc(2026, 9, 11, 10, 7, 0), utc(2026, 9, 11, 10, 5, 0)}, // ≡ @every 5m
	}
	for _, c := range cases {
		s := mustParse(t, c.spec)
		got, ok := s.lastOccurrenceAtOrBefore(c.at)
		if !ok {
			t.Errorf("%q at %v: ok = false, want %v", c.spec, c.at, c.want)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%q at %v = %v, want %v", c.spec, c.at, got, c.want)
		}
	}
}

func TestDomDowORRule(t *testing.T) {
	// "0 0 13 * 5": 13th of month AND every Friday.
	s := mustParse(t, "0 0 13 * 5")
	cases := []struct {
		at   time.Time // 2026-09-11 Fri 11th; 2026-09-13 Sun 13th; 2026-10-13 Tue 13th
		want time.Time
	}{
		{utc(2026, 9, 11, 12, 0, 0), utc(2026, 9, 11, 0, 0, 0)},   // Friday (dow)
		{utc(2026, 9, 13, 12, 0, 0), utc(2026, 9, 13, 0, 0, 0)},   // 13th (dom)
		{utc(2026, 10, 13, 12, 0, 0), utc(2026, 10, 13, 0, 0, 0)}, // 13th, Tuesday
		{utc(2026, 9, 14, 12, 0, 0), utc(2026, 9, 13, 0, 0, 0)},   // Monday: back to Sunday the 13th (dom)
	}
	for _, c := range cases {
		got, ok := s.lastOccurrenceAtOrBefore(c.at)
		if !ok || !got.Equal(c.want) {
			t.Errorf("at %v = %v, %v; want %v", c.at, got, ok, c.want)
		}
	}
	// A bare "*" side defers to the other: dom-only and dow-only.
	domOnly := mustParse(t, "0 0 13 * *")
	if got, _ := domOnly.lastOccurrenceAtOrBefore(utc(2026, 9, 11, 12, 0, 0)); !got.Equal(utc(2026, 8, 13, 0, 0, 0)) {
		t.Errorf("dom-only at Sep 11 = %v, want Aug 13", got)
	}
	dowOnly := mustParse(t, "0 0 * * 5")
	if got, _ := dowOnly.lastOccurrenceAtOrBefore(utc(2026, 9, 11, 12, 0, 0)); !got.Equal(utc(2026, 9, 11, 0, 0, 0)) {
		t.Errorf("dow-only at Sep 11 = %v, want Sep 11 00:00", got)
	}
	// "*/2" is restricted (not a bare "*"): OR rule still applies.
	stepped := mustParse(t, "0 0 */2 * 5")
	if got, _ := stepped.lastOccurrenceAtOrBefore(utc(2026, 9, 11, 12, 0, 0)); !got.Equal(utc(2026, 9, 11, 0, 0, 0)) {
		t.Errorf("stepped-dom OR at Sep 11 = %v, want Sep 11 00:00", got)
	}
}

func TestNeverOccurringScheduleIsClosed(t *testing.T) {
	s := mustParse(t, "0 0 30 2 *") // February 30 parses, never occurs
	if _, ok := s.lastOccurrenceAtOrBefore(utc(2026, 9, 11, 0, 0, 0)); ok {
		t.Error("Feb 30: ok = true, want false")
	}
	if WindowsOpen([]Schedule{s}, time.Hour, utc(2026, 9, 11, 0, 0, 0)) {
		t.Error("Feb 30: WindowsOpen = true, want false")
	}
}

func TestFeb29Horizon(t *testing.T) {
	s := mustParse(t, "0 0 29 2 *")
	got, ok := s.lastOccurrenceAtOrBefore(utc(2026, 9, 11, 0, 0, 0))
	if !ok || !got.Equal(utc(2024, 2, 29, 0, 0, 0)) {
		t.Errorf("Feb 29 at Sep 2026 = %v, %v; want 2024-02-29", got, ok)
	}
}

func TestEveryAnchorAcrossDays(t *testing.T) {
	s := mustParse(t, "@every 7h") // does not divide 24h: short overnight gap by design
	// 00,07,14,21 then next-day 00 (3h gap, documented).
	got, _ := s.lastOccurrenceAtOrBefore(utc(2026, 9, 11, 2, 0, 0))
	if !got.Equal(utc(2026, 9, 11, 0, 0, 0)) {
		t.Errorf("@every 7h at 02:00 = %v, want midnight", got)
	}
	// Handover stability: the same wall-clock instant gives the same
	// answer no matter when the process started (midnight anchor, not
	// process-start anchor).
	a := WindowsOpen([]Schedule{s}, 5*time.Minute, utc(2026, 9, 11, 7, 2, 0))
	if !a {
		t.Error("@every 7h at 07:02 + 5m grace: want open")
	}
}

func TestNumericAliasEqualsEvery(t *testing.T) {
	a := mustParse(t, "@300")
	b := mustParse(t, "@every 5m")
	at := utc(2026, 9, 11, 10, 7, 23)
	oa, oka := a.lastOccurrenceAtOrBefore(at)
	ob, okb := b.lastOccurrenceAtOrBefore(at)
	if !oka || !okb || !oa.Equal(ob) {
		t.Errorf("@300 vs @every 5m: %v/%v vs %v/%v", oa, oka, ob, okb)
	}
}

func TestWindowsOpenBoundaries(t *testing.T) {
	daily := mustParse(t, "@daily")
	grace := 5 * time.Minute
	open := []time.Time{
		utc(2026, 9, 11, 0, 0, 0),  // t == O
		utc(2026, 9, 11, 0, 4, 59), // inside
		utc(2026, 9, 12, 0, 0, 0),  // next day occurrence
		utc(2026, 9, 11, 0, 0, 30), // sub-minute after O
	}
	for _, at := range open {
		if !WindowsOpen([]Schedule{daily}, grace, at) {
			t.Errorf("WindowsOpen(@daily, 5m, %v) = false, want true", at)
		}
	}
	closed := []time.Time{
		utc(2026, 9, 11, 0, 5, 0),  // t == O+grace excluded
		utc(2026, 9, 11, 12, 0, 0), // far outside
		utc(2026, 9, 10, 23, 59, 0),
	}
	for _, at := range closed {
		if WindowsOpen([]Schedule{daily}, grace, at) {
			t.Errorf("WindowsOpen(@daily, 5m, %v) = true, want false", at)
		}
	}
}

func TestWindowsOpenUnionAndEdge(t *testing.T) {
	a := mustParse(t, "0 0 * * *")
	b := mustParse(t, "0 12 * * *")
	if !WindowsOpen([]Schedule{a, b}, 5*time.Minute, utc(2026, 9, 11, 12, 3, 0)) {
		t.Error("union: noon occurrence should open the window")
	}
	if WindowsOpen([]Schedule{a, b}, 5*time.Minute, utc(2026, 9, 11, 6, 0, 0)) {
		t.Error("union: 06:00 should be closed")
	}
	// Always-open edge (grace > period): documented operator error,
	// evaluated honestly.
	hourly := mustParse(t, "@hourly")
	if !WindowsOpen([]Schedule{hourly}, 2*time.Hour, utc(2026, 9, 11, 10, 30, 0)) {
		t.Error("grace > period: want open (operator error, not refused)")
	}
}

func TestWindowsOpenDegenerate(t *testing.T) {
	s := mustParse(t, "@daily")
	at := utc(2026, 9, 11, 0, 1, 0)
	if WindowsOpen(nil, time.Minute, at) {
		t.Error("no schedules: want closed")
	}
	if WindowsOpen([]Schedule{s}, 0, at) {
		t.Error("zero grace: want closed")
	}
	if WindowsOpen([]Schedule{s}, -time.Minute, at) {
		t.Error("negative grace: want closed")
	}
}

func TestNonUTCInput(t *testing.T) {
	s := mustParse(t, "@daily")
	// 02:00 at +02:00 == 00:00 UTC: the occurrence boundary is UTC.
	at := time.Date(2026, 9, 11, 2, 0, 0, 0, time.FixedZone("X", 2*3600))
	if !WindowsOpen([]Schedule{s}, 5*time.Minute, at) {
		t.Error("non-UTC input at a UTC occurrence: want open")
	}
	got, ok := s.lastOccurrenceAtOrBefore(at)
	if !ok || !got.Equal(utc(2026, 9, 11, 0, 0, 0)) {
		t.Errorf("non-UTC last occurrence = %v, %v", got, ok)
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, spec := range []string{"0 7 * * 3", "@daily", "@every 2h", "@300"} {
		if got := mustParse(t, spec).String(); got != spec {
			t.Errorf("String() = %q, want %q", got, spec)
		}
	}
}
