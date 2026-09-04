// Fire Air Hockey — server entry point.
//
// Pure-Go multiplayer air hockey server: static frontend, JSON lobby API and
// a stdlib-only WebSocket endpoint (see ws.go). Run with:
//
//	go run .                 # serves on :8080
//	go run . -addr :9090     # custom listen address
//
// Then open http://localhost:8080 in two browser windows to play.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "listen address (host:port)")
	flag.Parse()

	hub := NewHub()
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/rooms", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"rooms": hub.lobbyList()})
	})
	// Everything else is the frontend: exact files when they exist,
	// index.html otherwise (so invite links like /r/AB12CD deep-link).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" && !strings.HasPrefix(p, "r/") {
			if _, err := fs.Stat(sub, p); err == nil {
				http.ServeFileFS(w, r, sub, p)
				return
			}
		}
		http.ServeFileFS(w, r, sub, "index.html")
	})

	log.Printf("fire-airhockey serving on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
