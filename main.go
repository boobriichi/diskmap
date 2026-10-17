package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

var (
	scanner  = NewScanner()
	token    string
	lastPing time.Time
	pingMu   sync.Mutex
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
func sleepMs(ms int)      { time.Sleep(time.Duration(ms) * time.Millisecond) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func main() {
	b := make([]byte, 16)
	rand.Read(b)
	token = hex.EncodeToString(b)

	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("/api/drives", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, listDrives())
	}))

	mux.HandleFunc("/api/scan", authed(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if p == "" {
			http.Error(w, "path required", 400)
			return
		}
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			http.Error(w, "Folder not found", 404)
			return
		}
		scanner.Start(p)
		writeJSON(w, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/cancel", authed(func(w http.ResponseWriter, r *http.Request) {
		scanner.cancel.Store(true) // partial result will still be shown
		writeJSON(w, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/progress", authed(func(w http.ResponseWriter, r *http.Request) {
		scanner.mu.RLock()
		defer scanner.mu.RUnlock()
		writeJSON(w, map[string]any{
			"scanning": scanner.scanning,
			"ready":    scanner.root != nil,
			"root":     scanner.rootPath,
			"files":    scanner.files.Load(),
			"bytes":    scanner.bytes.Load(),
			"errors":   scanner.errors.Load(),
			"current":  scanner.current.Load(),
			"mode":     scanner.mode.Load(),
			"fastErr":  scanner.fastErr.Load(),
			"took":     scanner.took.Load(),
		})
	}))

	mux.HandleFunc("/api/tree", authed(func(w http.ResponseWriter, r *http.Request) {
		scanner.mu.RLock()
		defer scanner.mu.RUnlock()
		p := r.URL.Query().Get("path")
		if p == "" {
			p = scanner.rootPath
		}
		n := scanner.Find(p)
		if n == nil {
			http.Error(w, "not found", 404)
			return
		}
		depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
		if depth <= 0 || depth > 8 {
			depth = 4
		}
		out := Export(n, n.Path(), depth, n.Size, 0)
		writeJSON(w, map[string]any{"root": scanner.rootPath, "node": out})
	}))

	mux.HandleFunc("/api/delete", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		p := r.URL.Query().Get("path")
		scanner.mu.Lock()
		defer scanner.mu.Unlock()
		n := scanner.Find(p)
		if n == nil || n.Parent == nil {
			http.Error(w, "This item cannot be deleted", 400)
			return
		}
		if err := moveToRecycleBin(n.Path()); err != nil {
			http.Error(w, "Delete failed: "+err.Error(), 500)
			return
		}
		freed := n.Size
		scanner.Remove(n)
		writeJSON(w, map[string]any{"ok": true, "freed": freed})
	}))

	mux.HandleFunc("/api/open", authed(func(w http.ResponseWriter, r *http.Request) {
		if err := revealInExplorer(r.URL.Query().Get("path")); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}))

	mux.HandleFunc("/api/ping", authed(func(w http.ResponseWriter, r *http.Request) {
		pingMu.Lock()
		lastPing = time.Now()
		pingMu.Unlock()
		w.WriteHeader(204)
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + ln.Addr().String() + "/#" + token

	// Exit when the UI has been gone for a while (window closed).
	go func() {
		for {
			time.Sleep(5 * time.Second)
			pingMu.Lock()
			lp := lastPing
			pingMu.Unlock()
			if !lp.IsZero() && time.Since(lp) > 150*time.Second {
				os.Exit(0)
			}
		}
	}()

	go openWindow(url) // exits the process when the app window closes, if it can track it
	log.Println("DiskMap:", url)
	http.Serve(ln, mux)
}
