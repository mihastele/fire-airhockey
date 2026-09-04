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
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

func main() {
	addrFlag := flag.String("addr", "", "listen address (host:port); overrides $PORT")
	flag.Parse()
	listen := *addrFlag
	if listen == "" {
		listen = listenAddrFromEnv()
	}

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

	// Timeouts bound slow-client attacks (e.g. Slowloris): headers must
	// arrive fast, idle keep-alives are reaped. Hijacked WebSocket
	// connections are managed by ws.go deadlines instead.
	srv := &http.Server{
		Addr:              listen,
		Handler:           secureHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("fire-airhockey serving on %s", listen)
	log.Fatal(srv.ListenAndServe())
}

// listenAddrFromEnv resolves the listen address from $PORT (a bare port
// number such as 8080, or a full host:port), defaulting to :8080.
// Invalid values fail fast instead of binding something surprising.
func listenAddrFromEnv() string {
	p := strings.TrimSpace(os.Getenv("PORT"))
	if p == "" {
		return ":8080"
	}
	if strings.Contains(p, ":") {
		return p
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		log.Fatalf("invalid PORT %q: must be a port 1-65535 or host:port", p)
	}
	return ":" + p
}

// secureHeaders sets baseline hardening headers on every response. The
// frontend is same-origin files with no inline scripts, so a strict
// default-src 'self' policy fits; framing is denied to block clickjacking.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
