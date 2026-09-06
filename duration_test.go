package main

import (
	"testing"
	"time"
)

// envDuration is where durations enter tagbrr; this pins the str2duration
// behavior we rely on (d/w suffixes, Go forms, errors are fatal so only the
// happy paths are testable here).
func TestEnvDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"2w", 2 * 7 * 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"45m", 45 * time.Minute},
		{"", time.Minute}, // unset -> default
	}
	for _, c := range cases {
		t.Setenv("TAGBRR_TEST_DUR", c.in)
		if got := envDuration("TAGBRR_TEST_DUR", time.Minute); got != c.want {
			t.Errorf("envDuration(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
