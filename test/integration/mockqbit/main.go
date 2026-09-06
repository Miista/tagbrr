// mockqbit stands in for everything tagbrr talks to: qBittorrent's Web API
// (the three endpoints tagbrr touches) and an arr's history API, plus
// test-only endpoints so a scenario can arrange grabs and torrents and read
// back what was tagged.
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
	"time"
)

type history struct {
	EventType   string    `json:"eventType"`
	DownloadID  string    `json:"downloadId"`
	SourceTitle string    `json:"sourceTitle"`
	Date        time.Time `json:"date"`
	Data        struct {
		IndexerFlags string `json:"indexerFlags"`
	} `json:"data"`
}

type state struct {
	mu       sync.Mutex
	torrents map[string]bool
	tagged   map[string][]string
	grabs    []history
}

func main() {
	s := &state{torrents: map[string]bool{}, tagged: map[string][]string{}}
	// TORRENTS seeds hashes that exist in "qBittorrent" from the start.
	for _, h := range strings.Split(os.Getenv("TORRENTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			s.torrents[h] = true
		}
	}

	// --- qBittorrent ---
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

	// --- arr history ---
	http.HandleFunc("/api/v3/history/since", func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") == "" {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		since, err := time.Parse(time.RFC3339, r.URL.Query().Get("date"))
		if err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		out := []history{}
		for _, g := range s.grabs {
			if g.Date.After(since) {
				out = append(out, g)
			}
		}
		json.NewEncoder(rw).Encode(out)
	})

	// --- test-only ---
	// Record a grab in history: /testonly/grab?hash=X&flags=G_Freeleech,...
	http.HandleFunc("/testonly/grab", func(rw http.ResponseWriter, r *http.Request) {
		g := history{
			EventType:   "grabbed",
			DownloadID:  r.URL.Query().Get("hash"),
			SourceTitle: "it",
			Date:        time.Now(),
		}
		g.Data.IndexerFlags = r.URL.Query().Get("flags")
		s.mu.Lock()
		defer s.mu.Unlock()
		s.grabs = append(s.grabs, g)
	})
	// Make a torrent exist in "qBittorrent" after the fact.
	http.HandleFunc("/testonly/add", func(rw http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.torrents[r.URL.Query().Get("hash")] = true
	})
	// Everything tagged so far, for assertions.
	http.HandleFunc("/testonly/tagged", func(rw http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		json.NewEncoder(rw).Encode(s.tagged)
	})

	http.ListenAndServe(":8080", nil)
}
