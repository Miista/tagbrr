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

// Scenario is one test's world: a mock (qBittorrent + arr in one) and a
// stateless tagbrr polling it, joined by the suite network. The mock's test-only
// endpoints are published on a host loopback port so the test can arrange
// grabs and read back tags.
type Scenario struct {
	t       *testing.T
	mock    string // container name
	subject string // container name
	mockURL string // host URL of the mock
}

// Up starts a mock seeded with the given torrent hashes, writes the config,
// and starts tagbrr against the mock (as both its arr and its qBittorrent).
// Teardown is registered with the test, so the world is removed however the
// test ends.
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := &Scenario{t: t, mock: "tagbrr-it-mock", subject: "tagbrr-it-subject"}
	if err := os.WriteFile(filepath.Join(dir, "tagbrr.yaml"), []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	t.Cleanup(func() {
		docker("rm", "-f", s.mock, s.subject)
	})
	// A previous run may have left these behind.
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
		"-e", "TAGBRR_QBIT_URL=http://"+s.mock+":8080",
		"-e", "TAGBRR_QBIT_PASS=irrelevant",
		"-e", "TAGBRR_ARR_MOCK_URL=http://"+s.mock+":8080",
		"-e", "TAGBRR_ARR_MOCK_KEY=itkey",
		"-e", "TAGBRR_INTERVAL=1s",
		"-e", "TAGBRR_WINDOW=1h",
		"-v", filepath.Join(dir, "tagbrr.yaml")+":/config/tagbrr.yaml:ro",
		subjectImage); err != nil {
		t.Fatal(err)
	}

	s.mockURL = "http://" + s.hostPort(s.mock, "8080/tcp")
	s.waitHTTP(s.mockURL + "/testonly/tagged")
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

// waitHTTP waits until the URL answers at all — a refused connection means
// the process is not listening yet.
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

// Grab records a grabbed event in the mock arr's history, the way a real
// arr would after snatching a release.
func (s *Scenario) Grab(hash string, flags ...string) {
	s.t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/testonly/grab?hash=%s&flags=%s",
		s.mockURL, hash, strings.Join(flags, ",")))
	if err != nil {
		s.t.Fatal(err)
	}
	resp.Body.Close()
}

// AddTorrent makes a hash exist in the mock's qBittorrent after the fact —
// a slow add.
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
// Convergence-driven rather than a wall-clock guess: the poll interval is
// 1s, so the deadline is generous, and a pass returns as soon as the world
// settles.
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

// Settle gives the poller comfortably more than one pass. For asserting
// that something did NOT happen, waiting for convergence is impossible —
// there is nothing to converge to — so this is the honest alternative.
func (s *Scenario) Settle() {
	time.Sleep(3 * time.Second)
}
