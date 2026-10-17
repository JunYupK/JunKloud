package grader

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // zone names must resolve on any host
)

// cronSpec is a parsed 5-field cron schedule: the set of values each field
// allows, plus whether day-of-month and day-of-week were written as "*"
// (Kubernetes/cron treat "both restricted" as OR, so the star-ness is part
// of the meaning).
type cronSpec struct {
	tz               string
	fields           [5][]bool
	domStar, dowStar bool
	macro            string
}

var cronBounds = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}

var cronMonths = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var cronDays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

var cronMacros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

// parseCron parses a Kubernetes CronJob schedule (standard 5 fields, ranges,
// lists, steps, month/day names, @-macros, optional CRON_TZ=/TZ= prefix).
func parseCron(s string) (cronSpec, error) {
	var c cronSpec
	s = strings.TrimSpace(s)
	for _, p := range []string{"CRON_TZ=", "TZ="} {
		if strings.HasPrefix(s, p) {
			tz, rest, ok := strings.Cut(s, " ")
			if !ok {
				return c, fmt.Errorf("schedule %q has a time zone but no fields", s)
			}
			c.tz, s = strings.TrimPrefix(tz, p), strings.TrimSpace(rest)
			break
		}
	}
	if m, ok := cronMacros[strings.ToLower(s)]; ok {
		s = m
	}
	parts := strings.Fields(s)
	if len(parts) != 5 {
		return c, fmt.Errorf("schedule %q needs 5 fields", s)
	}
	for i, part := range parts {
		lo, hi := cronBounds[i][0], cronBounds[i][1]
		set := make([]bool, hi+1)
		star := false
		for _, item := range strings.Split(part, ",") {
			if err := cronItem(item, i, lo, hi, set, &star); err != nil {
				return c, fmt.Errorf("schedule %q field %d: %w", s, i+1, err)
			}
		}
		if i == 4 { // day of week: 7 is Sunday too
			if set[7] {
				set[0] = true
			}
			set[7] = false
		}
		c.fields[i] = set
		switch i {
		case 2:
			c.domStar = star
		case 4:
			c.dowStar = star
		}
	}
	return c, nil
}

func cronValue(s string, field int) (int, error) {
	l := strings.ToLower(s)
	if field == 3 {
		if v, ok := cronMonths[l]; ok {
			return v, nil
		}
	}
	if field == 4 {
		if v, ok := cronDays[l]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return v, nil
}

func cronItem(item string, field, lo, hi int, set []bool, star *bool) error {
	rng, stepText, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepText)
		if err != nil || n < 1 {
			return fmt.Errorf("bad step %q", stepText)
		}
		step = n
	}
	from, to := lo, hi
	switch {
	case rng == "*" || rng == "?":
		*star = true // like the CronJob controller: "*/n" also counts as a star
	case strings.Contains(rng, "-"):
		a, b, _ := strings.Cut(rng, "-")
		var err error
		if from, err = cronValue(a, field); err != nil {
			return err
		}
		if to, err = cronValue(b, field); err != nil {
			return err
		}
	default:
		v, err := cronValue(rng, field)
		if err != nil {
			return err
		}
		from, to = v, v
		if hasStep {
			to = hi
		}
	}
	if from < lo || to > hi || from > to {
		return fmt.Errorf("%q is out of range %d-%d", item, lo, hi)
	}
	for v := from; v <= to; v += step {
		set[v] = true
	}
	return nil
}

// dayMatches applies the day-of-month / day-of-week rule: when either field
// was written with a star both must match, otherwise either may.
func (c cronSpec) dayMatches(dom, dow int) bool {
	d, w := c.fields[2][dom], c.fields[4][dow]
	if c.domStar || c.dowStar {
		return d && w
	}
	return d || w
}

// sameTimes: the same minutes, hours, days and months (zones are not
// compared here).
func (c cronSpec) sameTimes(o cronSpec) bool {
	// Days are compared by their effect, not by how they were written: with
	// a star in either day field both must match, with none either may.
	for dom := 1; dom <= 31; dom++ {
		for dow := 0; dow <= 6; dow++ {
			if c.dayMatches(dom, dow) != o.dayMatches(dom, dow) {
				return false
			}
		}
	}
	for i := range c.fields {
		if i == 2 || i == 4 {
			continue
		}
		if len(c.fields[i]) != len(o.fields[i]) {
			return false
		}
		for v := range c.fields[i] {
			if c.fields[i][v] != o.fields[i][v] {
				return false
			}
		}
	}
	return true
}

// everyMinute: fires at every minute of every day, so no zone changes when.
func (c cronSpec) everyMinute() bool {
	for i, b := range cronBounds {
		hi := b[1]
		if i == 4 {
			hi = 6 // 7 is folded into 0
		}
		for v := b[0]; v <= hi; v++ {
			if !c.fields[i][v] {
				return false
			}
		}
	}
	return true
}

// cronEquivalent reports whether the live schedule (with the CronJob's
// spec.timeZone) fires at exactly the times of the expected one (with its
// declared zone). A zone is taken from CRON_TZ= when not given separately;
// none means UTC. A schedule that fires every minute is the same in any zone.
func cronEquivalent(expected, expectedZone, live, liveZone string) (bool, error) {
	x, err := parseCron(expected)
	if err != nil {
		return false, err
	}
	y, err := parseCron(live)
	if err != nil {
		return false, err
	}
	if !x.sameTimes(y) {
		return false, nil
	}
	xl, err := x.location(expectedZone)
	if err != nil {
		return false, err
	}
	yl, err := y.location(liveZone)
	if err != nil {
		return false, err
	}
	if x.everyMinute() {
		return true, nil
	}
	return sameZone(xl, yl), nil
}

// sameZone: both zones have the same UTC offset at 26 instants spread over
// two years (so "UTC" equals "Etc/UTC", and DST rules count).
func sameZone(a, b *time.Location) bool {
	for i := 0; i < 26; i++ {
		t := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, 28*i)
		_, oa := t.In(a).Zone()
		_, ob := t.In(b).Zone()
		if oa != ob {
			return false
		}
	}
	return true
}

// cronEqual reports whether two schedules (zones given as CRON_TZ=) fire at
// exactly the same times.
func cronEqual(a, b string) (bool, error) { return cronEquivalent(a, "", b, "") }

// location is the zone the schedule is read in: the CronJob's spec.timeZone
// (or CRON_TZ= in the schedule), else UTC, which is the controller manager's
// zone in the environment's nodes.
func (c cronSpec) location(specTimeZone string) (*time.Location, error) {
	name := specTimeZone
	if name == "" {
		name = c.tz
	}
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q", name)
	}
	return loc, nil
}

// matches reports whether the schedule fires at the minute t falls in.
func (c cronSpec) matches(t time.Time) bool {
	if !c.fields[0][t.Minute()] || !c.fields[1][t.Hour()] || !c.fields[3][int(t.Month())] {
		return false
	}
	return c.dayMatches(t.Day(), int(t.Weekday()))
}

// dueBetween lists the fire times in (from, to], in zone loc.
func (c cronSpec) dueBetween(from, to time.Time, loc *time.Location) []time.Time {
	var out []time.Time
	for t := from.In(loc).Truncate(time.Minute).Add(time.Minute); !t.After(to); t = t.Add(time.Minute) {
		if c.matches(t) {
			out = append(out, t)
		}
	}
	return out
}

// longGap stands for "no regular period within a week": longer than any
// observation window.
const longGap = 365 * 24 * time.Hour

// maxGap is the longest time between two consecutive fires, found over a
// reference week (the zone does not change gaps). Schedules that fire less
// than twice a week report longGap.
func (c cronSpec) maxGap() time.Duration {
	start := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC) // a Monday
	var prev time.Time
	var gap time.Duration
	n := 0
	for t := start; t.Before(start.AddDate(0, 0, 9)); t = t.Add(time.Minute) {
		if !c.matches(t) {
			continue
		}
		if n > 0 && t.Sub(prev) > gap {
			gap = t.Sub(prev)
		}
		prev = t
		n++
	}
	if n < 2 {
		return longGap
	}
	return gap
}
