// mockqbit stands in for qBittorrent's Web API — the three endpoints tagbrr
// touches, plus two of its own so a test can arrange torrents and read back
// what was tagged.
//
// Ordinary Go built into a scratch image by the suite, so it compiles and
// vets with everything else and nothing is pulled.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
)

type state struct {
	mu       sync.Mutex
	torrents map[string]bool
	tagged   map[string][]string
}

func main() {
	s := &state{torrents: map[string]bool{}, tagged: map[string][]string{}}
	// TORRENTS seeds hashes that exist from the start.
	for _, h := range strings.Split(os.Getenv("TORRENTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			s.torrents[h] = true
		}
	}

	http.HandleFunc("/api/v2/auth/login", func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte("Ok."))
	})
	http.HandleFunc("/api/v2/torrents/info", func(rw http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := []map[string]string{}
		for _, h := range strings.Split(r.URL.Query().Get("hashes"), "|") {
			if s.torrents[h] {
				out = append(out, map[string]string{"hash": h})
			}
		}
		json.NewEncoder(rw).Encode(out)
	})
	http.HandleFunc("/api/v2/torrents/addTags", func(rw http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		s.mu.Lock()
		defer s.mu.Unlock()
		h := r.Form.Get("hashes")
		for _, tag := range strings.Split(r.Form.Get("tags"), ",") {
			s.tagged[h] = append(s.tagged[h], tag)
		}
	})

	// Test-only: add a torrent after the fact, so a scenario can model a
	// slow qBittorrent add.
	http.HandleFunc("/testonly/add", func(rw http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.torrents[r.URL.Query().Get("hash")] = true
	})
	// Test-only: everything tagged so far, for assertions.
	http.HandleFunc("/testonly/tagged", func(rw http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		json.NewEncoder(rw).Encode(s.tagged)
	})

	http.ListenAndServe(":8080", nil)
}
