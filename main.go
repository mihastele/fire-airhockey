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
	"html"
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
	// index.html is rendered per request so SEO tags that require
	// absolute URLs (og:image, og:url, canonical) use $DOMAIN.
	siteBase := siteBaseFromEnv()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" && !strings.HasPrefix(p, "r/") {
			if _, err := fs.Stat(sub, p); err == nil {
				http.ServeFileFS(w, r, sub, p)
				return
			}
		}
		serveIndex(w, r, sub, siteBase)
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

// siteBaseFromEnv normalizes $DOMAIN into a scheme://host base URL with
// no trailing slash (e.g. "example.com" -> "https://example.com").
// Empty when DOMAIN is unset, in which case the request Host is used.
func siteBaseFromEnv() string {
	return normalizeDomain(os.Getenv("DOMAIN"))
}

// normalizeDomain maps a DOMAIN value to a scheme://host base URL.
// A missing scheme defaults to https; surrounding whitespace and
// trailing slashes are stripped. Empty input yields "".
func normalizeDomain(d string) string {
	d = strings.TrimSpace(d)
	d = strings.TrimRight(d, "/")
	if d == "" {
		return ""
	}
	if !strings.Contains(d, "://") {
		d = "https://" + d
	}
	return d
}

// requestBase returns the absolute site base for this request: $DOMAIN
// when set, otherwise the request's own scheme+host (honoring
// X-Forwarded-Proto/Host so deployments behind a proxy still emit
// correct absolute SEO URLs).
func requestBase(r *http.Request, siteBase string) string {
	if siteBase != "" {
		return siteBase
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); proto != "" {
		scheme = strings.ToLower(proto)
	}
	host := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	if host == "" {
		host = r.Host
	}
	host = strings.TrimRight(host, "/")
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// serveIndex serves index.html with relative SEO asset URLs rewritten to
// absolute ones (og:image/twitter:image require fully qualified URLs)
// plus per-page og:url and canonical tags.
func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS, siteBase string) {
	raw, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	base := requestBase(r, siteBase)
	out := renderIndexHTML(string(raw), r.URL.Path, base)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(out))
}

// renderIndexHTML rewrites index.html for one page: relative image URLs
// become absolute under base, and og:url/canonical point at the page.
// With an empty base the source is returned unchanged.
func renderIndexHTML(src, pagePath, base string) string {
	if base == "" {
		return src
	}
	out := strings.ReplaceAll(src, `content="/ogimg.png"`, `content="`+base+`/ogimg.png"`)
	pageURL := html.EscapeString(base + pagePath)
	if !strings.Contains(out, `property="og:url"`) {
		out = strings.Replace(out, "</head>",
			`<meta property="og:url" content="`+pageURL+`">`+"\n</head>", 1)
	}
	if !strings.Contains(out, `rel="canonical"`) {
		out = strings.Replace(out, "</head>",
			`<link rel="canonical" href="`+pageURL+`">`+"\n</head>", 1)
	}
	return out
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
