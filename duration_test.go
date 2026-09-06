package main

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "1d", want: 24 * time.Hour},
		{in: "2w", want: 2 * 7 * 24 * time.Hour},
		{in: "0.5d", want: 12 * time.Hour},
		{in: "168h", want: 168 * time.Hour},
		{in: "45m", want: 45 * time.Minute},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "30d", want: 30 * 24 * time.Hour},
		{in: "xd", wantErr: true},
		{in: "w", wantErr: true},
		{in: "1mo", wantErr: true}, // months deliberately unsupported
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := parseDuration(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseDuration(%q) accepted, want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDuration(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseDuration(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
