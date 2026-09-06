// tagbrr converges arr grab-time indexer flags onto qBittorrent tags.
//
// Sonarr/Radarr keep a release's indexer flags (freeleech, double upload,
// ...) on the grab event in their history. tagbrr polls that history,
// matches the flags against its rules, and tags the torrent in qBittorrent
// once it appears there — stateless: every pass re-reads a sliding window
// of history and tags only what is present and missing its tag. All seeding
// *policy* lives downstream (e.g. qui automations acting on the tags).
// Nothing is configured in the arrs: tagbrr only reads.
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
	"sort"

	"strings"

	"time"
	_ "time/tzdata" // embed tzdata so TZ works in the scratch image

	"github.com/rs/zerolog"
	str2duration "github.com/xhit/go-str2duration/v2"
	"gopkg.in/yaml.v3"
)

// The container owns this path and its internal port; they are not
// configuration. Mount the rules file at configPath.
const (
	listen     = ":9171" // health endpoint only
	configPath = "/config/tagbrr.yaml"
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

// Arr is one Sonarr/Radarr instance to poll, declared entirely in the
// environment: TAGBRR_ARR_<NAME>_URL and TAGBRR_ARR_<NAME>_KEY.
type Arr struct {
	Name string
	URL  string
	Key  string
}

// arrsFromEnv discovers the arrs to poll from TAGBRR_ARR_<NAME>_URL
// variables; each needs a matching _KEY. The name is kept lowercase for
// logs.
func arrsFromEnv(environ []string) ([]Arr, error) {
	vars := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	var arrs []Arr
	for k, v := range vars {
		if !strings.HasPrefix(k, "TAGBRR_ARR_") || !strings.HasSuffix(k, "_URL") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(k, "TAGBRR_ARR_"), "_URL")
		if name == "" || v == "" {
			return nil, fmt.Errorf("%s: empty arr name or URL", k)
		}
		key := vars["TAGBRR_ARR_"+name+"_KEY"]
		if key == "" {
			return nil, fmt.Errorf("arr %q: TAGBRR_ARR_%s_KEY is not set", strings.ToLower(name), name)
		}
		arrs = append(arrs, Arr{Name: strings.ToLower(name), URL: strings.TrimRight(v, "/"), Key: key})
	}
	if len(arrs) == 0 {
		return nil, fmt.Errorf("no arrs configured: set TAGBRR_ARR_<NAME>_URL and _KEY")
	}
	return arrs, nil
}

// Config is the rules file: a flat `rules` map ("flagA,flagB" -> tag). Any
// listed flag matching (case-insensitive substring) applies the tag.
type Config struct {
	Arrs  []Arr
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
	if len(cfg.Rules) == 0 {
		return Config{}, fmt.Errorf("the config file defines no rules")
	}
	// YAML maps have no order and Go randomizes iteration; sort so tag
	// order (in logs and in qBittorrent) is stable across runs.
	sort.Slice(cfg.Rules, func(i, j int) bool { return cfg.Rules[i].Tag < cfg.Rules[j].Tag })
	return cfg, nil
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

// candidate is one grabbed torrent whose flags matched a rule.
type candidate struct {
	Hash  string
	Tags  []string
	Title string
}

// poll reads each arr's grab history over the window and returns the
// torrents whose flags match a rule, keyed by lowercase hash. An arr that
// cannot be read is skipped for this pass; the window makes the next pass
// cover for it.
func poll(client *http.Client, cfg Config, window time.Duration) map[string]candidate {
	out := map[string]candidate{}
	for _, arr := range cfg.Arrs {
		records, err := grabsSince(client, arr, time.Now().Add(-window))
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
			if prev, ok := out[hash]; ok {
				tags = mergeTags(prev.Tags, tags)
			}
			out[hash] = candidate{Hash: hash, Tags: tags, Title: r.SourceTitle}
		}
	}
	return out
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
	// qBittorrent 4.x answers a successful login with 200 and "Ok."; 5.x
	// with 204 and an empty body. Bad credentials are 200+"Fails." on 4.x
	// and 401 on 5.x.
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode == http.StatusOK && strings.Contains(string(b), "Ok"):
		return nil
	}
	return fmt.Errorf("login failed: status %d, body %q", resp.StatusCode, string(b))
}

// tags returns, for each of the given hashes that exists in qBittorrent,
// the tags it currently carries.
func (q *qbit) tags(hashes []string) (map[string][]string, error) {
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
		Tags string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&torrents); err != nil {
		return nil, err
	}
	found := map[string][]string{}
	for _, t := range torrents {
		var tags []string
		for _, tag := range strings.Split(t.Tags, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				tags = append(tags, tag)
			}
		}
		found[strings.ToLower(t.Hash)] = tags
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

func reconcile(q *qbit, want map[string]candidate) {
	if len(want) == 0 {
		return
	}
	var hashes []string
	for h := range want {
		hashes = append(hashes, h)
	}
	have, err := q.tags(hashes)
	if err != nil {
		// One relogin attempt per pass; qBit sessions expire.
		if lerr := q.login(); lerr != nil {
			logger.Warn().Msgf("reconcile pass failed (%v) and relogin also failed (%v); will retry next pass", err, lerr)
			return
		}
		if have, err = q.tags(hashes); err != nil {
			logger.Warn().Msgf("reconcile pass failed after relogin (%v); will retry next pass", err)
			return
		}
	}
	for h, c := range want {
		existing, present := have[h]
		if !present {
			continue // not in qBittorrent (yet, or ever); the window retries
		}
		has := map[string]bool{}
		for _, t := range existing {
			has[t] = true
		}
		var missing []string
		for _, t := range c.Tags {
			if !has[t] {
				missing = append(missing, t)
			}
		}
		if len(missing) == 0 {
			continue // converged
		}
		if err := q.addTags(h, missing); err != nil {
			logger.Warn().Msgf("could not tag %s (%s): %v; will retry next pass", h, c.Title, err)
			continue
		}
		logger.Info().Msgf("tagged %s with %v (%s)", h, missing, c.Title)
	}
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
	d, err := str2duration.ParseDuration(v)
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
		interval = envDuration("TAGBRR_INTERVAL", 15*time.Minute)
		window   = envDuration("TAGBRR_WINDOW", 720*time.Hour)
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
	if cfg.Arrs, err = arrsFromEnv(os.Environ()); err != nil {
		logger.Fatal().Msgf("%v", err)
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
		logger.Info().Msgf("tagbrr %s polling %s every %s with %d rules over a %s window",
			version, strings.Join(names, ", "), interval, len(cfg.Rules), window)
		for {
			reconcile(q, poll(arrClient, cfg, window))
			time.Sleep(interval)
		}
	}()

	logger.Fatal().Err(http.ListenAndServe(listen, nil)).Msg("http server stopped")
}
