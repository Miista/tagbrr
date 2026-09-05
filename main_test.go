package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testRules = []Rule{
	{Flags: []string{"doubleupload"}, Tag: "du"},
	{Flags: []string{"freeleech", "halfleech"}, Tag: "fl"},
}

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig([]byte("rules:\n  doubleupload: du\n  freeleech, halfleech: fl\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(cfg.Rules))
	}
	for _, r := range cfg.Rules {
		if r.Tag == "fl" && (len(r.Flags) != 2 || r.Flags[1] != "halfleech") {
			t.Errorf("fl rule flags = %v, want [freeleech halfleech] (whitespace trimmed)", r.Flags)
		}
	}
	if _, err := parseConfig([]byte("rules:\n  \" , \": x\n")); err == nil {
		t.Error("empty flag list accepted")
	}
	if _, err := parseConfig([]byte("rules:\n  freeleech: \"\"\n")); err == nil {
		t.Error("empty tag accepted")
	}
	if _, err := parseConfig([]byte(": not yaml: [")); err == nil {
		t.Error("invalid yaml accepted")
	}
}

func TestMatchTags(t *testing.T) {
	cases := []struct {
		name  string
		flags []string
		want  []string
	}{
		{"gazelle prefix", []string{"G_DoubleUpload"}, []string{"du"}},
		{"lowercase", []string{"doubleupload"}, []string{"du"}},
		{"both flags", []string{"G_Freeleech", "doubleupload"}, []string{"du", "fl"}},
		{"no match", []string{"internal", "scene"}, nil},
		{"empty flags", nil, nil},
		{"duplicate flags collapse", []string{"freeleech", "G_Freeleech"}, []string{"fl"}},
		{"alternate flag same rule", []string{"Halfleech"}, []string{"fl"}},
		{"both alternates still one tag", []string{"freeleech", "halfleech"}, []string{"fl"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matchTags(testRules, c.flags)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("matchTags(%v) = %v, want %v", c.flags, got, c.want)
			}
		})
	}
}

func newTestWatchlist(t *testing.T) *Watchlist {
	t.Helper()
	w, err := loadWatchlist(filepath.Join(t.TempDir(), "watchlist.json"))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWatchlistPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchlist.json")
	w, err := loadWatchlist(path)
	if err != nil {
		t.Fatal(err)
	}
	w.add(Entry{Hash: "aaa", Tags: []string{"du"}, Added: time.Now()})
	w.add(Entry{Hash: "aaa", Tags: []string{"fl"}, Added: time.Now()}) // re-grab merges tags
	w.add(Entry{Hash: "bbb", Tags: []string{"fl"}, Added: time.Now()})

	w2, err := loadWatchlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(w2.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(w2.Entries))
	}
	if got := strings.Join(w2.Entries["aaa"].Tags, ","); got != "du,fl" {
		t.Errorf("merged tags = %q, want du,fl", got)
	}

	w2.remove([]string{"aaa"})
	w3, _ := loadWatchlist(path)
	if _, ok := w3.Entries["aaa"]; ok {
		t.Error("aaa still present after remove")
	}
	if _, ok := w3.Entries["bbb"]; !ok {
		t.Error("bbb lost by remove of aaa")
	}
}

func postWebhook(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestWebhookHandler(t *testing.T) {
	watch := newTestWatchlist(t)
	poke := make(chan struct{}, 1)
	h := webhookHandler(Config{Rules: testRules}, watch, poke)

	// GET is rejected
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}

	// invalid JSON is rejected
	if rec := postWebhook(t, h, "{nope"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON = %d, want 400", rec.Code)
	}

	// Test event acked, not watched
	if rec := postWebhook(t, h, `{"eventType":"Test"}`); rec.Code != http.StatusOK {
		t.Errorf("Test event = %d, want 200", rec.Code)
	}
	// Grab without matching flags acked, not watched
	postWebhook(t, h, `{"eventType":"Grab","downloadId":"FFFF","release":{"indexerFlags":["internal"]}}`)
	// Grab without downloadId acked, not watched
	postWebhook(t, h, `{"eventType":"Grab","release":{"indexerFlags":["freeleech"]}}`)
	if n := len(watch.snapshot()); n != 0 {
		t.Fatalf("watchlist has %d entries after non-matching events, want 0", n)
	}

	// matching Grab is watched, hash lowercased, poke fired
	rec = postWebhook(t, h, `{"eventType":"Grab","downloadId":"ABCD1234","release":{"releaseTitle":"X","indexerFlags":["G_DoubleUpload"]}}`)
	if rec.Code != http.StatusOK {
		t.Errorf("Grab = %d, want 200", rec.Code)
	}
	entries := watch.snapshot()
	if len(entries) != 1 || entries[0].Hash != "abcd1234" || strings.Join(entries[0].Tags, ",") != "du" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	select {
	case <-poke:
	default:
		t.Error("reconciler was not poked")
	}
}

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			rw.WriteHeader(http.StatusOK)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	if got := healthcheck(listen); got != 0 {
		t.Errorf("healthcheck against live server = %d, want 0", got)
	}
	srv.Close()
	if got := healthcheck(listen); got != 1 {
		t.Errorf("healthcheck against closed server = %d, want 1", got)
	}
	if got := healthcheck("not-an-address"); got != 1 {
		t.Errorf("healthcheck with bad listen addr = %d, want 1", got)
	}
}

// fakeQbit implements the three qBittorrent endpoints reconcile touches.
type fakeQbit struct {
	torrents map[string]bool     // hashes present in the client
	tagged   map[string][]string // hash -> tags applied
	logins   int
}

func (f *fakeQbit) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(rw http.ResponseWriter, r *http.Request) {
		f.logins++
		rw.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(rw http.ResponseWriter, r *http.Request) {
		var out []map[string]string
		for _, h := range strings.Split(r.URL.Query().Get("hashes"), "|") {
			if f.torrents[h] {
				out = append(out, map[string]string{"hash": h})
			}
		}
		json.NewEncoder(rw).Encode(out)
	})
	mux.HandleFunc("/api/v2/torrents/addTags", func(rw http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		h := r.Form.Get("hashes")
		f.tagged[h] = append(f.tagged[h], strings.Split(r.Form.Get("tags"), ",")...)
	})
	return httptest.NewServer(mux)
}

func TestReconcile(t *testing.T) {
	fake := &fakeQbit{torrents: map[string]bool{"inqbit": true}, tagged: map[string][]string{}}
	srv := fake.server()
	defer srv.Close()

	watch := newTestWatchlist(t)
	now := time.Now()
	watch.add(Entry{Hash: "inqbit", Tags: []string{"du", "fl"}, Added: now})   // present -> tag + drop
	watch.add(Entry{Hash: "pending", Tags: []string{"du"}, Added: now})        // absent, fresh -> keep
	watch.add(Entry{Hash: "stale", Tags: []string{"fl"}, Added: now.Add(-72 * time.Hour)}) // absent, old -> expire

	q := newQbit(srv.URL, "admin", "pw")
	reconcile(q, watch, 48*time.Hour)

	if got := strings.Join(fake.tagged["inqbit"], ","); got != "du,fl" {
		t.Errorf("tags applied = %q, want du,fl", got)
	}
	if len(fake.tagged) != 1 {
		t.Errorf("tagged %d torrents, want 1: %v", len(fake.tagged), fake.tagged)
	}

	remaining := watch.snapshot()
	if len(remaining) != 1 || remaining[0].Hash != "pending" {
		t.Errorf("remaining = %+v, want only pending", remaining)
	}

	// second pass: nothing new should happen
	reconcile(q, watch, 48*time.Hour)
	if got := len(fake.tagged["inqbit"]); got != 2 { // "du","fl" from the single call
		t.Errorf("inqbit tag calls changed unexpectedly: %v", fake.tagged["inqbit"])
	}
	if len(watch.snapshot()) != 1 {
		t.Error("pending entry lost on idle pass")
	}
}

func TestReconcileRelogin(t *testing.T) {
	fake := &fakeQbit{torrents: map[string]bool{"aaa": true}, tagged: map[string][]string{}}
	mux := http.NewServeMux()
	authed := false
	mux.HandleFunc("/api/v2/auth/login", func(rw http.ResponseWriter, r *http.Request) {
		fake.logins++
		authed = true
		rw.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(rw http.ResponseWriter, r *http.Request) {
		if !authed {
			rw.WriteHeader(http.StatusForbidden)
			return
		}
		json.NewEncoder(rw).Encode([]map[string]string{{"hash": "aaa"}})
	})
	mux.HandleFunc("/api/v2/torrents/addTags", func(rw http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		fake.tagged[r.Form.Get("hashes")] = strings.Split(r.Form.Get("tags"), ",")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	watch := newTestWatchlist(t)
	watch.add(Entry{Hash: "aaa", Tags: []string{"du"}, Added: time.Now()})

	q := newQbit(srv.URL, "admin", "pw")
	reconcile(q, watch, 48*time.Hour) // first info call 403s -> relogin -> retry succeeds

	if fake.logins != 1 {
		t.Errorf("logins = %d, want 1", fake.logins)
	}
	if strings.Join(fake.tagged["aaa"], ",") != "du" {
		t.Errorf("tags = %v, want [du]", fake.tagged["aaa"])
	}
	if len(watch.snapshot()) != 0 {
		t.Error("entry not removed after successful tag")
	}
}

func TestQbitLoginFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte("Fails.")) // qBit returns 200 with "Fails." on bad credentials
	}))
	defer srv.Close()
	q := newQbit(srv.URL, "admin", "wrong")
	if err := q.login(); err == nil {
		t.Error("login succeeded against Fails. response")
	}
}
