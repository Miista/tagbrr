//go:build integration

package integration

import "testing"

const rules = "rules:\n  doubleupload: du\n  freeleech,halfleech: fl\n"

// A grab whose torrent already exists in qBittorrent converges to its tags.
func TestGrabAlreadyInClient(t *testing.T) {
	s := Up(t, rules, "aaaa1111")
	s.Grab("AAAA1111", "G_Freeleech", "doubleupload") // arr sends uppercase hashes
	s.WaitTagged("aaaa1111", "du", "fl")
}

// A grab whose torrent shows up in qBittorrent only later still converges —
// the watch survives until the add happens.
func TestGrabBeforeClientKnows(t *testing.T) {
	s := Up(t, rules)
	s.Grab("BBBB2222", "freeleech")
	s.Settle() // reconciler runs against a client without the torrent
	if got := s.Tagged()["bbbb2222"]; got != nil {
		t.Fatalf("tagged %v before the torrent existed", got)
	}
	s.AddTorrent("bbbb2222")
	s.WaitTagged("bbbb2222", "fl")
}

// A grab with no matching flags, and a Test event, tag nothing.
func TestNothingToTag(t *testing.T) {
	s := Up(t, rules, "cccc3333")
	s.Grab("CCCC3333", "internal", "scene")
	s.Settle()
	if got := s.Tagged(); len(got) != 0 {
		t.Fatalf("tagged %v, want nothing", got)
	}
}
