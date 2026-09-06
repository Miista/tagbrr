//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Scenario is one test's world: a mock qBittorrent and a tagbrr, joined by
// the suite network, with tagbrr's webhook published on a host loopback
// port so the test can play the arr.
type Scenario struct {
	t       *testing.T
	mock    string // container name
	subject string // container name
	hook    string // host URL of tagbrr's webhook
	mockURL string // host URL of the mock's test-only endpoints
}

// Up starts a mock seeded with the given torrent hashes, writes the rules
// file, and starts tagbrr against the mock. Teardown is registered with the
// test, so the world is removed however the test ends.
func Up(t *testing.T, rules string, seeded ...string) *Scenario {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}

	// The testbed lives in the repository, not $TMPDIR: on macOS that is a
	// symlink, and docker records the resolved path, which then never
	// matches the one the test holds.
	dir := filepath.Join(root, "testbed")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o777); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(dir, "data"), 0o777) // tagbrr runs as nobody
	if err := os.WriteFile(filepath.Join(dir, "tagbrr.yaml"), []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	s := &Scenario{t: t, mock: "tagbrr-it-mock", subject: "tagbrr-it-subject"}
	t.Cleanup(func() {
		docker("rm", "-f", s.mock, s.subject)
	})
	// A previous run may have left these behind (sweep only knows names via
	// the label, but be explicit anyway).
	docker("rm", "-f", s.mock, s.subject)

	if _, err := docker("run", "-d", "--rm", "--name", s.mock,
		"--label", label+"=1", "--network", network,
		"-p", "127.0.0.1:0:8080",
		"-e", "TORRENTS="+strings.Join(seeded, ","),
		mockImage); err != nil {
		t.Fatal(err)
	}
	if _, err := docker("run", "-d", "--rm", "--name", s.subject,
		"--label", label+"=1", "--network", network,
		"-p", "127.0.0.1:0:9171",
		"-e", "TAGBRR_QBIT_URL=http://"+s.mock+":8080",
		"-e", "TAGBRR_QBIT_PASS=irrelevant",
		"-e", "TAGBRR_INTERVAL=1s",
		"-v", filepath.Join(dir, "tagbrr.yaml")+":/config/tagbrr.yaml:ro",
		"-v", filepath.Join(dir, "data")+":/data",
		subjectImage); err != nil {
		t.Fatal(err)
	}

	s.hook = "http://" + s.hostPort(s.subject, "9171/tcp") + "/webhook"
	s.mockURL = "http://" + s.hostPort(s.mock, "8080/tcp")
	s.waitHTTP(s.hook)          // any response means tagbrr is up
	s.waitHTTP(s.mockURL + "/") // and the mock too
	return s
}

func (s *Scenario) hostPort(container, port string) string {
	s.t.Helper()
	out, err := docker("port", container, port)
	if err != nil {
		s.t.Fatal(err)
	}
	// Possibly one line per address family; the first will do.
	return strings.Fields(out)[0]
}

// waitHTTP waits until the URL answers at all. What it answers is not the
// point — a refused connection means the process is not listening yet.
func (s *Scenario) waitHTTP(url string) {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatalf("%s never came up", url)
}

// Grab plays the arr: it POSTs an On Grab webhook for the given hash and
// flags and requires the 200 the arr would require.
func (s *Scenario) Grab(hash string, flags ...string) {
	s.t.Helper()
	body := fmt.Sprintf(`{"eventType":"Grab","downloadId":%q,`+
		`"release":{"releaseTitle":"it","indexerFlags":%s}}`,
		hash, mustJSON(flags))
	resp, err := http.Post(s.hook, "application/json", strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("webhook answered %d, want 200", resp.StatusCode)
	}
}

// AddTorrent makes a hash exist in the mock after the fact — a slow
// qBittorrent add.
func (s *Scenario) AddTorrent(hash string) {
	s.t.Helper()
	resp, err := http.Get(s.mockURL + "/testonly/add?hash=" + hash)
	if err != nil {
		s.t.Fatal(err)
	}
	resp.Body.Close()
}

// Tagged reads back everything the mock has been asked to tag.
func (s *Scenario) Tagged() map[string][]string {
	s.t.Helper()
	resp, err := http.Get(s.mockURL + "/testonly/tagged")
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// WaitTagged waits until the hash carries exactly the given tags, or fails.
// Convergence-driven rather than a wall-clock guess: the reconcile interval
// is 1s, so the deadline is generous, and a pass returns as soon as the
// world settles.
func (s *Scenario) WaitTagged(hash string, tags ...string) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last []string
	for time.Now().Before(deadline) {
		last = s.Tagged()[hash]
		if strings.Join(last, ",") == strings.Join(tags, ",") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatalf("%s tagged %v, want %v", hash, last, tags)
}

// Settle gives the reconciler comfortably more than one pass. For asserting
// that something did NOT happen, waiting for convergence is impossible —
// there is nothing to converge to — so this is the honest alternative.
func (s *Scenario) Settle() {
	time.Sleep(3 * time.Second)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
