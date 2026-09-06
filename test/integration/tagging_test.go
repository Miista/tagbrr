//go:build integration

package integration

import "testing"

const rules = "rules:\n  doubleupload: du\n  freeleech,halfleech: fl\n"

// A grab whose torrent already exists in qBittorrent converges to its tags.
// The arr reports uppercase hashes; qBittorrent speaks lowercase.
func TestGrabAlreadyInClient(t *testing.T) {
	s := Up(t, rules, "aaaa1111")
	s.Grab("AAAA1111", "G_Freeleech", "doubleupload")
	s.WaitTagged("aaaa1111", "du", "fl")
}

// A grab whose torrent shows up in qBittorrent only later still converges —
// the pending entry survives until the add happens.
func TestGrabBeforeClientKnows(t *testing.T) {
	s := Up(t, rules)
	s.Grab("BBBB2222", "freeleech")
	s.Settle() // poller and reconciler run against a client without the torrent
	if got := s.Tagged()["bbbb2222"]; got != nil {
		t.Fatalf("tagged %v before the torrent existed", got)
	}
	s.AddTorrent("bbbb2222")
	s.WaitTagged("bbbb2222", "fl")
}

// A flagless grab and one with unmatched flags tag nothing.
func TestNothingToTag(t *testing.T) {
	s := Up(t, rules, "cccc3333", "dddd4444")
	s.Grab("CCCC3333", "internal", "scene")
	s.Grab("DDDD4444", "0")
	s.Settle()
	if got := s.Tagged(); len(got) != 0 {
		t.Fatalf("tagged %v, want nothing", got)
	}
}
