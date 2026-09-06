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

const testConfig = `
arrs:
  radarr: http://radarr:7878/
rules:
  doubleupload: du
  freeleech, halfleech: fl
`

func testEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestParseConfig(t *testing.T) {
	env := testEnv(map[string]string{"TAGBRR_ARR_RADARR_KEY": "secret"})

	cfg, err := parseConfig([]byte(testConfig), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Arrs) != 1 || cfg.Arrs[0].Name != "radarr" || cfg.Arrs[0].Key != "secret" {
		t.Fatalf("arrs = %+v", cfg.Arrs)
	}
	if cfg.Arrs[0].URL != "http://radarr:7878" {
		t.Errorf("trailing slash kept: %q", cfg.Arrs[0].URL)
	}
	if len(cfg.Rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(cfg.Rules))
	}
	for _, r := range cfg.Rules {
		if r.Tag == "fl" && (len(r.Flags) != 2 || r.Flags[1] != "halfleech") {
			t.Errorf("fl rule flags = %v, want [freeleech halfleech] (whitespace trimmed)", r.Flags)
		}
	}

	// missing API key env is a config error
	if _, err := parseConfig([]byte(testConfig), testEnv(nil)); err == nil {
		t.Error("missing TAGBRR_ARR_RADARR_KEY accepted")
	}
	// no arrs at all
	if _, err := parseConfig([]byte("rules:\n  freeleech: fl\n"), env); err == nil {
		t.Error("config without arrs accepted")
	}
	// no rules at all
	if _, err := parseConfig([]byte("arrs:\n  radarr: http://r\n"), env); err == nil {
		t.Error("config without rules accepted")
	}
	// empty flag list / empty tag / invalid yaml
	if _, err := parseConfig([]byte("arrs:\n  radarr: http://r\nrules:\n  \" , \": x\n"), env); err == nil {
		t.Error("empty flag list accepted")
	}
	if _, err := parseConfig([]byte("arrs:\n  radarr: http://r\nrules:\n  freeleech: \"\"\n"), env); err == nil {
		t.Error("empty tag accepted")
	}
	if _, err := parseConfig([]byte(": not yaml: ["), env); err == nil {
		t.Error("invalid yaml accepted")
	}
}

func TestSplitFlags(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"G_Freeleech", []string{"G_Freeleech"}},
		{"G_Freeleech, G_DoubleUpload", []string{"G_Freeleech", "G_DoubleUpload"}},
		{"0", nil}, // history's "no flags"
		{"", nil},
	}
	for _, c := range cases {
		if got := splitFlags(c.in); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("splitFlags(%q) = %v, want %v", c.in, got, c.want)
		}
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

func newTestState(t *testing.T) *State {
	t.Helper()
	s, err := loadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStatePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	s.add(Entry{Hash: "aaa", Tags: []string{"du"}, Added: time.Now()})
	s.add(Entry{Hash: "aaa", Tags: []string{"fl"}, Added: time.Now()}) // re-grab merges tags
	s.add(Entry{Hash: "bbb", Tags: []string{"fl"}, Added: time.Now()})
	mark := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	s.setLastPolled("radarr", mark)

	s2, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(s2.Entries))
	}
	if got := strings.Join(s2.Entries["aaa"].Tags, ","); got != "du,fl" {
		t.Errorf("merged tags = %q, want du,fl", got)
	}
	if got, ok := s2.lastPolled("radarr"); !ok || !got.Equal(mark) {
		t.Errorf("lastPolled = %v %v, want %v", got, ok, mark)
	}
	if _, ok := s2.lastPolled("sonarr"); ok {
		t.Error("lastPolled for unpolled arr should be absent")
	}

	s2.remove([]string{"aaa"})
	s3, _ := loadState(path)
	if _, ok := s3.Entries["aaa"]; ok {
		t.Error("aaa still present after remove")
	}
	if _, ok := s3.Entries["bbb"]; !ok {
		t.Error("bbb lost by remove of aaa")
	}
}

// fakeArr serves the one history endpoint poll uses.
type fakeArr struct {
	records []grabRecord
	lastKey string
	lastQ   string
	status  int
}

func (f *fakeArr) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		f.lastKey = r.Header.Get("X-Api-Key")
		f.lastQ = r.URL.RawQuery
		if r.URL.Path != "/api/v3/history/since" {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		if f.status != 0 {
			rw.WriteHeader(f.status)
			return
		}
		json.NewEncoder(rw).Encode(f.records)
	}))
}

func record(hash, flags string) grabRecord {
	var r grabRecord
	r.EventType = "grabbed"
	r.DownloadID = hash
	r.SourceTitle = "title-" + hash
	r.Date = time.Now()
	r.Data.IndexerFlags = flags
	return r
}

func TestPoll(t *testing.T) {
	arr := &fakeArr{records: []grabRecord{
		record("AAAA", "G_Freeleech"),
		record("BBBB", "0"),                                       // flagless -> ignored
		record("CCCC", "G_Freeleech, G_DoubleUpload"),             // both tags
		{EventType: "downloadFolderImported", DownloadID: "DDDD"}, // wrong type -> ignored
		record("", "G_Freeleech"),                                 // no hash -> ignored
	}}
	srv := arr.server()
	defer srv.Close()

	state := newTestState(t)
	cfg := Config{
		Arrs:  []Arr{{Name: "radarr", URL: srv.URL, Key: "sekrit"}},
		Rules: testRules,
	}
	poll(srv.Client(), cfg, state, 48*time.Hour)

	if arr.lastKey != "sekrit" {
		t.Errorf("api key sent = %q", arr.lastKey)
	}
	if !strings.Contains(arr.lastQ, "eventType=grabbed") {
		t.Errorf("query = %q, want eventType=grabbed", arr.lastQ)
	}
	entries := map[string]Entry{}
	for _, e := range state.snapshot() {
		entries[e.Hash] = e
	}
	if len(entries) != 2 {
		t.Fatalf("queued %v, want aaaa and cccc", entries)
	}
	if got := strings.Join(entries["aaaa"].Tags, ","); got != "fl" {
		t.Errorf("aaaa tags = %q", got)
	}
	if got := strings.Join(entries["cccc"].Tags, ","); got != "du,fl" {
		t.Errorf("cccc tags = %q", got)
	}
	if _, ok := state.lastPolled("radarr"); !ok {
		t.Error("lastPolled not recorded after a successful poll")
	}

	// a failing arr must not advance lastPolled
	arr.status = http.StatusInternalServerError
	before, _ := state.lastPolled("radarr")
	poll(srv.Client(), cfg, state, 48*time.Hour)
	after, _ := state.lastPolled("radarr")
	if !after.Equal(before) {
		t.Error("lastPolled advanced past a failed poll")
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

	state := newTestState(t)
	now := time.Now()
	state.add(Entry{Hash: "inqbit", Tags: []string{"du", "fl"}, Added: now})               // present -> tag + drop
	state.add(Entry{Hash: "pending", Tags: []string{"du"}, Added: now})                    // absent, fresh -> keep
	state.add(Entry{Hash: "stale", Tags: []string{"fl"}, Added: now.Add(-72 * time.Hour)}) // absent, old -> expire

	q := newQbit(srv.URL, "admin", "pw")
	reconcile(q, state, 48*time.Hour)

	if got := strings.Join(fake.tagged["inqbit"], ","); got != "du,fl" {
		t.Errorf("tags applied = %q, want du,fl", got)
	}
	if len(fake.tagged) != 1 {
		t.Errorf("tagged %d torrents, want 1: %v", len(fake.tagged), fake.tagged)
	}

	remaining := state.snapshot()
	if len(remaining) != 1 || remaining[0].Hash != "pending" {
		t.Errorf("remaining = %+v, want only pending", remaining)
	}

	// second pass: nothing new should happen
	reconcile(q, state, 48*time.Hour)
	if got := len(fake.tagged["inqbit"]); got != 2 { // "du","fl" from the single call
		t.Errorf("inqbit tag calls changed unexpectedly: %v", fake.tagged["inqbit"])
	}
	if len(state.snapshot()) != 1 {
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

	state := newTestState(t)
	state.add(Entry{Hash: "aaa", Tags: []string{"du"}, Added: time.Now()})

	q := newQbit(srv.URL, "admin", "pw")
	reconcile(q, state, 48*time.Hour) // first info call 403s -> relogin -> retry succeeds

	if fake.logins != 1 {
		t.Errorf("logins = %d, want 1", fake.logins)
	}
	if strings.Join(fake.tagged["aaa"], ",") != "du" {
		t.Errorf("tags = %v, want [du]", fake.tagged["aaa"])
	}
	if len(state.snapshot()) != 0 {
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

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			rw.WriteHeader(http.StatusOK)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if got := healthcheck(addr); got != 0 {
		t.Errorf("healthcheck against live server = %d, want 0", got)
	}
	srv.Close()
	if got := healthcheck(addr); got != 1 {
		t.Errorf("healthcheck against closed server = %d, want 1", got)
	}
	if got := healthcheck("not-an-address"); got != 1 {
		t.Errorf("healthcheck with bad listen addr = %d, want 1", got)
	}
}
