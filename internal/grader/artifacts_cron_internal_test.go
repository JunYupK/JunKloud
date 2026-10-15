package grader

import (
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/testutil"
)

func TestCronEquivalence(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"*/5 * * * *", "0,5,10,15,20,25,30,35,40,45,50,55 * * * *", true},
		{"@hourly", "0 * * * *", true},
		{"*/1 * * * *", "* * * * *", true},
		{"0 0 * * 7", "0 0 * * 0", true},
		{"0 0 1 * *", "0 0 * * 1", false},
		{"CRON_TZ=Asia/Seoul 0 9 * * *", "0 9 * * *", false},
	} {
		same, err := cronEqual(tc.a, tc.b)
		testutil.NoError(t, err)
		testutil.Equal(t, same, tc.same)
	}
}

func TestCronDueTimes(t *testing.T) {
	utc := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	spec := func(s string) cronSpec {
		c, err := parseCron(s)
		testutil.NoError(t, err)
		return c
	}
	due := func(s string, from time.Time, d time.Duration, loc *time.Location) []time.Time {
		return spec(s).dueBetween(from, from.Add(d), loc)
	}
	// every minute: boundaries strictly after the start, up to and including the end
	got := due("* * * * *", utc, 90*time.Second, time.UTC)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, got[0], utc.Add(time.Minute))
	// the start exactly on a boundary is not due (it already fired before)
	testutil.Equal(t, len(due("* * * * *", utc, 59*time.Second, time.UTC)), 0)
	testutil.Equal(t, len(due("* * * * *", utc, 60*time.Second, time.UTC)), 1)
	testutil.Equal(t, len(due("*/5 * * * *", utc, 120*time.Second, time.UTC)), 0)
	// time zones
	seoul, err := time.LoadLocation("Asia/Seoul")
	testutil.NoError(t, err)
	testutil.Equal(t, len(due("1 9 * * *", utc, 90*time.Second, seoul)), 1)
	testutil.Equal(t, len(due("1 9 * * *", utc, 90*time.Second, time.UTC)), 0)
	// day-of-month and day-of-week: both restricted means OR, a star means AND
	thu := time.Date(2026, 10, 15, 23, 59, 30, 0, time.UTC)                     // a Thursday; midnight starts Friday the 16th
	testutil.Equal(t, len(due("0 0 16 * 1", thu, 60*time.Second, time.UTC)), 1) // the 16th OR a Monday
	testutil.Equal(t, len(due("0 0 * * 1", thu, 60*time.Second, time.UTC)), 0)  // Fridays are not Mondays
	testutil.Equal(t, len(due("0 0 * * 5", thu, 60*time.Second, time.UTC)), 1)
}

func TestCronMaxGap(t *testing.T) {
	for _, tc := range []struct {
		s   string
		gap time.Duration
	}{
		{"* * * * *", time.Minute},
		{"*/2 * * * *", 2 * time.Minute},
		{"0,1 * * * *", 59 * time.Minute},
		{"0 * * * *", time.Hour},
		{"0 0 * * *", 24 * time.Hour},
	} {
		c, err := parseCron(tc.s)
		testutil.NoError(t, err)
		testutil.Equal(t, c.maxGap(), tc.gap)
	}
	c, _ := parseCron("0 0 1 1 *")
	testutil.Equal(t, c.maxGap() > 24*time.Hour*7, true) // not once a week: treated as longer than any window
}

func TestCronEquivalentWithZones(t *testing.T) {
	for _, tc := range []struct {
		expected, expZone, live, liveZone string
		same                              bool
	}{
		{"0 9 * * *", "Asia/Seoul", "0 9 * * *", "Asia/Seoul", true},
		{"0 9 * * *", "Asia/Seoul", "CRON_TZ=Asia/Seoul 0 9 * * *", "", true},
		{"0 9 * * *", "", "0 9 * * *", "UTC", true},
		{"0 9 * * *", "", "0 9 * * *", "Etc/UTC", true},
		{"0 9 * * *", "", "0 9 * * *", "Asia/Seoul", false},
		{"0 9 * * *", "Asia/Seoul", "0 9 * * *", "", false},
		{"@daily", "", "0 0 * * *", "", true},
		{"@daily", "", "0 1 * * *", "", false},
		{"* * * * *", "Asia/Seoul", "*/1 * * * *", "America/New_York", true}, // every minute: zone-free
		{"* * * * *", "", "* * * * 1-5", "", false},
		{"*/2 * * * *", "", "* * * * *", "", false},
	} {
		same, err := cronEquivalent(tc.expected, tc.expZone, tc.live, tc.liveZone)
		testutil.NoError(t, err)
		if same != tc.same {
			t.Errorf("%q [%s] vs %q [%s] = %t, want %t", tc.expected, tc.expZone, tc.live, tc.liveZone, same, tc.same)
		}
	}
	if _, err := cronEquivalent("* * * * *", "", "* * * * *", "Mars/Olympus"); err == nil {
		t.Error("an unknown zone must be an error")
	}
}
