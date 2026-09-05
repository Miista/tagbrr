// tagbrr converges arr grab-time indexer flags onto qBittorrent tags.
//
// Sonarr/Radarr POST their On Grab webhook here; matching flags put the
// torrent hash on a persistent watch list. A reconcile loop tags the
// torrent in qBittorrent once it appears, then drops the entry. Entries
// that never appear expire after a TTL. All seeding *policy* lives
// downstream (e.g. qui automations acting on the tags).
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

// Config is a flat map: "flagA,flagB" -> tag. Any listed flag matching
// (case-insensitive substring) applies the tag.
type Config struct {
	Rules []Rule
}

func parseConfig(b []byte) (Config, error) {
	var raw struct {
		Rules map[string]string `yaml:"rules"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return Config{}, err
	}
	var cfg Config
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
	return cfg, nil
}

type Entry struct {
	Hash  string    `json:"hash"`
	Tags  []string  `json:"tags"`
	Title string    `json:"title"`
	Added time.Time `json:"added"`
}

type Watchlist struct {
	mu      sync.Mutex
	path    string
	Entries map[string]Entry `json:"entries"`
}

func loadWatchlist(path string) (*Watchlist, error) {
	w := &Watchlist{path: path, Entries: map[string]Entry{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	return w, json.Unmarshal(b, w)
}

// persist writes the watch list atomically (temp file + rename).
// Callers must hold w.mu.
func (w *Watchlist) persist() {
	b, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		logger.Error().Err(err).Msg("could not marshal the watch list")
		return
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		logger.Error().Err(err).Msg("could not write the watch list file")
		return
	}
	if err := os.Rename(tmp, w.path); err != nil {
		logger.Error().Err(err).Msg("could not move the watch list file into place")
	}
}

func (w *Watchlist) add(e Entry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if old, ok := w.Entries[e.Hash]; ok {
		e.Tags = mergeTags(old.Tags, e.Tags)
		e.Added = old.Added
	}
	w.Entries[e.Hash] = e
	w.persist()
}

func (w *Watchlist) remove(hashes []string) {
	if len(hashes) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, h := range hashes {
		delete(w.Entries, h)
	}
	w.persist()
}

func (w *Watchlist) snapshot() []Entry {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Entry, 0, len(w.Entries))
	for _, e := range w.Entries {
		out = append(out, e)
	}
	return out
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

// grabEvent is the subset of the Sonarr/Radarr webhook payload we consume.
type grabEvent struct {
	EventType  string `json:"eventType"`
	DownloadID string `json:"downloadId"`
	Release    struct {
		ReleaseTitle string   `json:"releaseTitle"`
		IndexerFlags []string `json:"indexerFlags"`
	} `json:"release"`
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

func reconcile(q *qbit, w *Watchlist, ttl time.Duration) {
	entries := w.snapshot()
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
	w.remove(done)
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
		os.Exit(healthcheck(envOr("TAGBRR_LISTEN", ":9171")))
	}
	logger = newLogger(envOr("LOG_LEVEL", "info"))
	var (
		qbtURL     = os.Getenv("TAGBRR_QBIT_URL")
		qbtUser    = envOr("TAGBRR_QBIT_USER", "admin")
		qbtPass    = os.Getenv("TAGBRR_QBIT_PASS")
		listen     = envOr("TAGBRR_LISTEN", ":9171")
		interval   = envDuration("TAGBRR_INTERVAL", 2*time.Minute)
		ttl        = envDuration("TAGBRR_TTL", 48*time.Hour)
		configPath = envOr("TAGBRR_CONFIG", "/config/tagbrr.yaml")
		dataPath   = envOr("TAGBRR_DATA", "/data/watchlist.json")
	)
	if qbtURL == "" || qbtPass == "" {
		logger.Fatal().Msg("TAGBRR_QBIT_URL and TAGBRR_QBIT_PASS are required")
	}

	cfgBytes, err := os.ReadFile(configPath)
	if err != nil {
		logger.Fatal().Msgf("could not read the config file: %v", err)
	}
	cfg, err := parseConfig(cfgBytes)
	if err != nil {
		logger.Fatal().Msgf("could not parse the config file: %v", err)
	}
	if len(cfg.Rules) == 0 {
		logger.Fatal().Msg("the config file defines no rules")
	}

	if err := os.MkdirAll(filepath.Dir(dataPath), 0o755); err != nil {
		logger.Fatal().Msgf("could not create the data directory: %v", err)
	}
	watch, err := loadWatchlist(dataPath)
	if err != nil {
		logger.Fatal().Msgf("could not load the watch list: %v", err)
	}

	q := newQbit(qbtURL, qbtUser, qbtPass)
	poke := make(chan struct{}, 1)

	http.HandleFunc("/webhook", webhookHandler(cfg, watch, poke))
	http.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	go func() {
		ticker := time.NewTicker(interval)
		for {
			select {
			case <-ticker.C:
			case <-poke:
				time.Sleep(5 * time.Second) // give the arr a moment to hand qBit the torrent
			}
			reconcile(q, watch, ttl)
		}
	}()

	logger.Info().Msgf("tagbrr %s listening on %s with %d rules (reconcile every %s, watch entries expire after %s)", version, listen, len(cfg.Rules), interval, ttl)
	logger.Fatal().Err(http.ListenAndServe(listen, nil)).Msg("http server stopped")
}

func webhookHandler(cfg Config, watch *Watchlist, poke chan struct{}) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(rw, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var ev grabEvent
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ev); err != nil {
			http.Error(rw, "bad payload", http.StatusBadRequest)
			return
		}
		rw.WriteHeader(http.StatusOK) // always ack; the arr retries on non-2xx
		if ev.EventType != "Grab" || ev.DownloadID == "" {
			return
		}
		tags := matchTags(cfg.Rules, ev.Release.IndexerFlags)
		if len(tags) == 0 {
			return
		}
		watch.add(Entry{
			Hash:  strings.ToLower(ev.DownloadID),
			Tags:  tags,
			Title: ev.Release.ReleaseTitle,
			Added: time.Now(),
		})
		logger.Info().Msgf("watching %s for tags %v (%s)", strings.ToLower(ev.DownloadID), tags, ev.Release.ReleaseTitle)
		select {
		case poke <- struct{}{}:
		default:
		}
	}
}
