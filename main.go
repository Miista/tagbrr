// tagbrr converges arr grab-time indexer flags onto qBittorrent tags.
//
// Sonarr/Radarr keep a release's indexer flags (freeleech, double upload,
// ...) on the grab event in their history. tagbrr polls that history,
// matches the flags against its rules, and tags the torrent in qBittorrent
// once it appears there. Pending torrents that never appear expire after a
// TTL. All seeding *policy* lives downstream (e.g. qui automations acting
// on the tags). Nothing is configured in the arrs: tagbrr only reads.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // embed tzdata so TZ works in the scratch image

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

// The container owns these paths and its internal port; they are not
// configuration. Mount the rules file at configPath and a volume at the
// data directory.
const (
	listen     = ":9171" // health endpoint only
	configPath = "/config/tagbrr.yaml"
	dataPath   = "/data/state.json"
)

// version is injected by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

// logger is replaced in main() with a LOG_LEVEL-aware instance; the
// default here keeps tests and early init working.
var logger zerolog.Logger = newLogger("info")

// Rule maps any of a set of flag patterns to one tag.
type Rule struct {
	Flags []string
	Tag   string
}

// Arr is one Sonarr/Radarr instance to poll. The API key comes from the
// environment (TAGBRR_ARR_<NAME>_KEY), not the config file.
type Arr struct {
	Name string
	URL  string
	Key  string
}

// Config is the rules file: an `arrs` map (name -> base URL) and a flat
// `rules` map ("flagA,flagB" -> tag). Any listed flag matching
// (case-insensitive substring) applies the tag.
type Config struct {
	Arrs  []Arr
	Rules []Rule
}

func parseConfig(b []byte, getenv func(string) string) (Config, error) {
	var raw struct {
		Arrs  map[string]string `yaml:"arrs"`
		Rules map[string]string `yaml:"rules"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return Config{}, err
	}
	var cfg Config
	for name, base := range raw.Arrs {
		envKey := "TAGBRR_ARR_" + strings.ToUpper(name) + "_KEY"
		key := getenv(envKey)
		if key == "" {
			return Config{}, fmt.Errorf("arr %q: %s is not set", name, envKey)
		}
		cfg.Arrs = append(cfg.Arrs, Arr{Name: name, URL: strings.TrimRight(base, "/"), Key: key})
	}
	if len(cfg.Arrs) == 0 {
		return Config{}, fmt.Errorf("the config file defines no arrs")
	}
	for flags, tag := range raw.Rules {
		var pats []string
		for _, f := range strings.Split(flags, ",") {
			if f = strings.TrimSpace(f); f != "" {
				pats = append(pats, f)
			}
		}
		if len(pats) == 0 || strings.TrimSpace(tag) == "" {
			return Config{}, fmt.Errorf("rule %q: empty flags or tag", flags)
		}
		cfg.Rules = append(cfg.Rules, Rule{Flags: pats, Tag: strings.TrimSpace(tag)})
	}
	if len(cfg.Rules) == 0 {
		return Config{}, fmt.Errorf("the config file defines no rules")
	}
	return cfg, nil
}

// Entry is one grabbed torrent waiting to be tagged in qBittorrent.
type Entry struct {
	Hash  string    `json:"hash"`
	Tags  []string  `json:"tags"`
	Title string    `json:"title"`
	Added time.Time `json:"added"`
}

// State is everything tagbrr remembers across restarts: torrents pending a
// tag, and how far into each arr's history it has already looked.
type State struct {
	mu         sync.Mutex
	path       string
	Entries    map[string]Entry     `json:"entries"`
	LastPolled map[string]time.Time `json:"lastPolled"`
}

func loadState(path string) (*State, error) {
	s := &State{path: path, Entries: map[string]Entry{}, LastPolled: map[string]time.Time{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Entries == nil {
		s.Entries = map[string]Entry{}
	}
	if s.LastPolled == nil {
		s.LastPolled = map[string]time.Time{}
	}
	return s, nil
}

// persist writes the state atomically (temp file + rename).
// Callers must hold s.mu.
func (s *State) persist() {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		logger.Error().Err(err).Msg("could not marshal the state")
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		logger.Error().Err(err).Msg("could not write the state file")
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		logger.Error().Err(err).Msg("could not move the state file into place")
	}
}

func (s *State) add(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.Entries[e.Hash]; ok {
		e.Tags = mergeTags(old.Tags, e.Tags)
		e.Added = old.Added
	}
	s.Entries[e.Hash] = e
	s.persist()
}

func (s *State) remove(hashes []string) {
	if len(hashes) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range hashes {
		delete(s.Entries, h)
	}
	s.persist()
}

func (s *State) snapshot() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.Entries))
	for _, e := range s.Entries {
		out = append(out, e)
	}
	return out
}

func (s *State) lastPolled(arr string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.LastPolled[arr]
	return t, ok
}

func (s *State) setLastPolled(arr string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastPolled[arr] = t
	s.persist()
}

func mergeTags(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range append(a, b...) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// splitFlags turns history's indexerFlags string ("G_Freeleech" or
// "G_Freeleech, G_DoubleUpload"; "0" when none) into individual flags.
func splitFlags(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" && f != "0" {
			out = append(out, f)
		}
	}
	return out
}

func matchTags(rules []Rule, flags []string) []string {
	var tags []string
	for _, r := range rules {
	rule:
		for _, pat := range r.Flags {
			for _, f := range flags {
				if strings.Contains(strings.ToLower(f), strings.ToLower(pat)) {
					tags = append(tags, r.Tag)
					break rule
				}
			}
		}
	}
	return mergeTags(nil, tags)
}

// grabRecord is the subset of an arr history record we consume.
type grabRecord struct {
	EventType   string    `json:"eventType"`
	DownloadID  string    `json:"downloadId"`
	SourceTitle string    `json:"sourceTitle"`
	Date        time.Time `json:"date"`
	Data        struct {
		IndexerFlags string `json:"indexerFlags"`
	} `json:"data"`
}

// grabsSince asks one arr for its grabbed history since the given time.
func grabsSince(client *http.Client, arr Arr, since time.Time) ([]grabRecord, error) {
	u := fmt.Sprintf("%s/api/v3/history/since?date=%s&eventType=grabbed",
		arr.URL, url.QueryEscape(since.UTC().Format(time.RFC3339)))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", arr.Key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", arr.Name, resp.StatusCode)
	}
	var records []grabRecord
	if err := json.NewDecoder(resp.Body).Decode(&records); err != nil {
		return nil, err
	}
	return records, nil
}

// pollOverlap is re-read on every poll so a record written just before the
// previous poll's timestamp is never missed. Re-tagging is idempotent, so
// the overlap costs nothing.
const pollOverlap = 10 * time.Minute

// poll reads each arr's grab history and queues matching torrents.
func poll(client *http.Client, cfg Config, state *State, backfill time.Duration) {
	for _, arr := range cfg.Arrs {
		since, ok := state.lastPolled(arr.Name)
		if !ok {
			since = time.Now().Add(-backfill)
		} else {
			since = since.Add(-pollOverlap)
		}
		start := time.Now()
		records, err := grabsSince(client, arr, since)
		if err != nil {
			logger.Warn().Msgf("could not read %s's history (%v); will retry next pass", arr.Name, err)
			continue
		}
		for _, r := range records {
			if !strings.EqualFold(r.EventType, "grabbed") || r.DownloadID == "" {
				continue
			}
			tags := matchTags(cfg.Rules, splitFlags(r.Data.IndexerFlags))
			if len(tags) == 0 {
				continue
			}
			hash := strings.ToLower(r.DownloadID)
			state.add(Entry{Hash: hash, Tags: tags, Title: r.SourceTitle, Added: time.Now()})
			logger.Info().Msgf("queued %s from %s for tags %v (%s)", hash, arr.Name, tags, r.SourceTitle)
		}
		state.setLastPolled(arr.Name, start)
	}
}

type qbit struct {
	base   string
	user   string
	pass   string
	client *http.Client
}

func newQbit(base, user, pass string) *qbit {
	jar, _ := cookiejar.New(nil)
	return &qbit{base: base, user: user, pass: pass, client: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

func (q *qbit) login() error {
	resp, err := q.client.PostForm(q.base+"/api/v2/auth/login",
		url.Values{"username": {q.user}, "password": {q.pass}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "Ok") {
		return fmt.Errorf("login failed: status %d, body %q", resp.StatusCode, string(b))
	}
	return nil
}

// present returns which of the given hashes exist in qBittorrent.
func (q *qbit) present(hashes []string) (map[string]bool, error) {
	resp, err := q.client.Get(q.base + "/api/v2/torrents/info?hashes=" + url.QueryEscape(strings.Join(hashes, "|")))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 {
		return nil, fmt.Errorf("forbidden (session expired)")
	}
	var torrents []struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&torrents); err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, t := range torrents {
		found[strings.ToLower(t.Hash)] = true
	}
	return found, nil
}

func (q *qbit) addTags(hash string, tags []string) error {
	resp, err := q.client.PostForm(q.base+"/api/v2/torrents/addTags",
		url.Values{"hashes": {hash}, "tags": {strings.Join(tags, ",")}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("addTags: status %d", resp.StatusCode)
	}
	return nil
}

func reconcile(q *qbit, s *State, ttl time.Duration) {
	entries := s.snapshot()
	if len(entries) == 0 {
		return
	}
	var hashes []string
	for _, e := range entries {
		hashes = append(hashes, e.Hash)
	}
	found, err := q.present(hashes)
	if err != nil {
		// One relogin attempt per pass; qBit sessions expire.
		if lerr := q.login(); lerr != nil {
			logger.Warn().Msgf("reconcile pass failed (%v) and relogin also failed (%v); will retry next pass", err, lerr)
			return
		}
		if found, err = q.present(hashes); err != nil {
			logger.Warn().Msgf("reconcile pass failed after relogin (%v); will retry next pass", err)
			return
		}
	}
	var done []string
	for _, e := range entries {
		switch {
		case found[e.Hash]:
			if err := q.addTags(e.Hash, e.Tags); err != nil {
				logger.Warn().Msgf("could not tag %s (%s): %v; will retry next pass", e.Hash, e.Title, err)
				continue // retry next pass
			}
			logger.Info().Msgf("tagged %s with %v (%s)", e.Hash, e.Tags, e.Title)
			done = append(done, e.Hash)
		case time.Since(e.Added) > ttl:
			logger.Info().Msgf("gave up on %s after %s; it never appeared in qBittorrent (%s)", e.Hash, ttl, e.Title)
			done = append(done, e.Hash)
		}
	}
	s.remove(done)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Fatal().Msgf("invalid duration in %s: %v", key, err)
	}
	return d
}

// healthcheck probes our own /healthz; used as the Docker HEALTHCHECK
// command since the scratch image has no curl.
func healthcheck(listen string) int {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 1
	}
	if host == "" {
		host = "127.0.0.1"
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	resp.Body.Close()
	return 0
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck(listen))
	}
	logger = newLogger(envOr("LOG_LEVEL", "info"))
	var (
		qbtURL   = os.Getenv("TAGBRR_QBIT_URL")
		qbtUser  = envOr("TAGBRR_QBIT_USER", "admin")
		qbtPass  = os.Getenv("TAGBRR_QBIT_PASS")
		interval = envDuration("TAGBRR_INTERVAL", 2*time.Minute)
		ttl      = envDuration("TAGBRR_TTL", 48*time.Hour)
		backfill = envDuration("TAGBRR_BACKFILL", 48*time.Hour)
	)
	if qbtURL == "" || qbtPass == "" {
		logger.Fatal().Msg("TAGBRR_QBIT_URL and TAGBRR_QBIT_PASS are required")
	}

	cfgBytes, err := os.ReadFile(configPath)
	if err != nil {
		logger.Fatal().Msgf("could not read the config file: %v", err)
	}
	cfg, err := parseConfig(cfgBytes, os.Getenv)
	if err != nil {
		logger.Fatal().Msgf("could not parse the config file: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(dataPath), 0o755); err != nil {
		logger.Fatal().Msgf("could not create the data directory: %v", err)
	}
	state, err := loadState(dataPath)
	if err != nil {
		logger.Fatal().Msgf("could not load the state: %v", err)
	}

	q := newQbit(qbtURL, qbtUser, qbtPass)
	arrClient := &http.Client{Timeout: 30 * time.Second}

	http.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	go func() {
		var names []string
		for _, a := range cfg.Arrs {
			names = append(names, a.Name)
		}
		logger.Info().Msgf("tagbrr %s polling %s every %s with %d rules (backfill %s, pending torrents expire after %s)",
			version, strings.Join(names, ", "), interval, len(cfg.Rules), backfill, ttl)
		for {
			poll(arrClient, cfg, state, backfill)
			reconcile(q, state, ttl)
			time.Sleep(interval)
		}
	}()

	logger.Fatal().Err(http.ListenAndServe(listen, nil)).Msg("http server stopped")
}
