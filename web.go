// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

package main

// Graphical interface: a small local web page shown in its own app window.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

//go:embed ui/index.html
var indexHTML []byte

//go:embed ui/icon.svg
var iconSVG []byte

const uiPort = 24880

var lastPoll atomic.Int64

// startUI serves the interface. Returns the URL, or "" with an error.
func startUI(b *Bridge) (string, error) {
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", uiPort))
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/icon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(iconSVG)
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		lastPoll.Store(time.Now().UnixMilli())
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		s := snapshot(after)
		s.ToEden, s.ToSwitch = b.data.Counts()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("/api/firewall", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		go fixFirewallElevated()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		go func() {
			time.Sleep(200 * time.Millisecond)
			closeAppWindow()
			os.Exit(0)
		}()
	})
	srv := &http.Server{Handler: localOnly(mux), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	return fmt.Sprintf("http://127.0.0.1:%d/", uiPort), nil
}

// localOnly rejects requests that do not come from this PC or whose Host
// header is not ours (protects against other web pages calling the API).
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host != "127.0.0.1" || r.Host != fmt.Sprintf("127.0.0.1:%d", uiPort) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}
