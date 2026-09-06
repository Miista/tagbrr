package main

import (
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testRules = []Rule{
	{Flags: []string{"doubleupload"}, Tag: "du"},
	{Flags: []string{"freeleech", "halfleech"}, Tag: "fl"},
}

const testConfig = `
rules:
  doubleupload: du
  freeleech, halfleech: fl
`

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig([]byte(testConfig))
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

	// no rules at all
	if _, err := parseConfig([]byte("{}")); err == nil {
		t.Error("config without rules accepted")
	}
	// empty flag list / empty tag / invalid yaml
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

func TestArrsFromEnv(t *testing.T) {
	arrs, err := arrsFromEnv([]string{
		"TAGBRR_ARR_RADARR_URL=http://radarr:7878/",
		"TAGBRR_ARR_RADARR_KEY=secret",
		"PATH=/usr/bin", // unrelated vars ignored
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(arrs) != 1 || arrs[0].Name != "radarr" || arrs[0].Key != "secret" {
		t.Fatalf("arrs = %+v", arrs)
	}
	if arrs[0].URL != "http://radarr:7878" {
		t.Errorf("trailing slash kept: %q", arrs[0].URL)
	}

	// URL without a matching key is an error
	if _, err := arrsFromEnv([]string{"TAGBRR_ARR_RADARR_URL=http://r"}); err == nil {
		t.Error("missing TAGBRR_ARR_RADARR_KEY accepted")
	}
	// no arrs at all is an error
	if _, err := arrsFromEnv([]string{"PATH=/usr/bin"}); err == nil {
		t.Error("no arrs accepted")
	}
	// empty URL is an error
	if _, err := arrsFromEnv([]string{"TAGBRR_ARR_RADARR_URL="}); err == nil {
		t.Error("empty URL accepted")
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

	cfg := Config{
		Arrs:  []Arr{{Name: "radarr", URL: srv.URL, Key: "sekrit"}},
		Rules: testRules,
	}
	got := poll(srv.Client(), cfg, 48*time.Hour)

	if arr.lastKey != "sekrit" {
		t.Errorf("api key sent = %q", arr.lastKey)
	}
	if !strings.Contains(arr.lastQ, "eventType=grabbed") {
		t.Errorf("query = %q, want eventType=grabbed", arr.lastQ)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %v, want aaaa and cccc", got)
	}
	if tags := strings.Join(got["aaaa"].Tags, ","); tags != "fl" {
		t.Errorf("aaaa tags = %q", tags)
	}
	if tags := strings.Join(got["cccc"].Tags, ","); tags != "du,fl" {
		t.Errorf("cccc tags = %q", tags)
	}

	// a failing arr yields no candidates rather than an error
	arr.status = http.StatusInternalServerError
	if got := poll(srv.Client(), cfg, 48*time.Hour); len(got) != 0 {
		t.Errorf("failing arr produced candidates: %v", got)
	}
}

// fakeQbit implements the three qBittorrent endpoints reconcile touches.
// torrents maps hash -> current tags; addTags mutates it, like the real one.
type fakeQbit struct {
	torrents map[string][]string
	calls    int
	logins   int
}

func (f *fakeQbit) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(rw http.ResponseWriter, r *http.Request) {
		f.logins++
		rw.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(rw http.ResponseWriter, r *http.Request) {
		var out []map[string]string
		for _, h := range strings.Split(r.URL.Query().Get("hashes"), "|") {
			if tags, ok := f.torrents[h]; ok {
				out = append(out, map[string]string{"hash": h, "tags": strings.Join(tags, ", ")})
			}
		}
		json.NewEncoder(rw).Encode(out)
	})
	mux.HandleFunc("/api/v2/torrents/addTags", func(rw http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.calls++
		h := r.Form.Get("hashes")
		f.torrents[h] = append(f.torrents[h], strings.Split(r.Form.Get("tags"), ",")...)
	})
	return mux
}

func candidates(cs ...candidate) map[string]candidate {
	out := map[string]candidate{}
	for _, c := range cs {
		out[c.Hash] = c
	}
	return out
}

func TestReconcile(t *testing.T) {
	fake := &fakeQbit{torrents: map[string][]string{
		"bare":    {},           // present, untagged -> gets both tags
		"partial": {"fl"},       // present, half tagged -> gets only du
		"done":    {"du", "fl"}, // present, converged -> untouched
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	want := candidates(
		candidate{Hash: "bare", Tags: []string{"du", "fl"}},
		candidate{Hash: "partial", Tags: []string{"du", "fl"}},
		candidate{Hash: "done", Tags: []string{"du", "fl"}},
		candidate{Hash: "absent", Tags: []string{"du"}}, // not in qBit -> skipped
	)
	q := newQbit(srv.URL, "admin", "pw")
	reconcile(q, want)

	if got := strings.Join(fake.torrents["bare"], ","); got != "du,fl" {
		t.Errorf("bare = %q, want du,fl", got)
	}
	if got := strings.Join(fake.torrents["partial"], ","); got != "fl,du" {
		t.Errorf("partial = %q, want fl,du (only the missing tag added)", got)
	}
	if fake.calls != 2 {
		t.Errorf("addTags called %d times, want 2 (done and absent untouched)", fake.calls)
	}
	if _, ok := fake.torrents["absent"]; ok {
		t.Error("absent torrent materialized")
	}

	// second pass: everything converged, no calls at all
	reconcile(q, want)
	if fake.calls != 2 {
		t.Errorf("idle pass made %d extra addTags calls", fake.calls-2)
	}
}

func TestReconcileRelogin(t *testing.T) {
	fake := &fakeQbit{torrents: map[string][]string{"aaa": {}}}
	authed := false
	mux := http.NewServeMux()
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
		json.NewEncoder(rw).Encode([]map[string]string{{"hash": "aaa", "tags": ""}})
	})
	mux.HandleFunc("/api/v2/torrents/addTags", func(rw http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		fake.torrents[r.Form.Get("hashes")] = strings.Split(r.Form.Get("tags"), ",")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	q := newQbit(srv.URL, "admin", "pw")
	// first info call 403s -> relogin -> retry succeeds
	reconcile(q, candidates(candidate{Hash: "aaa", Tags: []string{"du"}}))

	if fake.logins != 1 {
		t.Errorf("logins = %d, want 1", fake.logins)
	}
	if got := strings.Join(fake.torrents["aaa"], ","); got != "du" {
		t.Errorf("tags = %q, want du", got)
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
