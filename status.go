// SPDX-License-Identifier: GPL-3.0-or-later
// Puente Eden <-> Switch (ldn_mitm)

package main

// Live status shown by the graphical interface.

import (
	"strings"
	"sync"
	"time"
)

type LogLine struct {
	ID   int    `json:"id"`
	Time string `json:"t"`
	Text string `json:"m"`
	Kind string `json:"k"` // info, ok, warn
}

type Status struct {
	Version   string `json:"version"`
	Admin     bool   `json:"admin"`
	Firewall  string `json:"firewall"`
	PCIP      string `json:"pcIP"`
	RoomPort  int    `json:"roomPort"`
	RoomReady bool   `json:"roomReady"`
	RoomError string `json:"roomError"`
	RoomMode  string `json:"roomMode"` // "server" or "client"

	EdenNick string `json:"edenNick"`
	EdenIP   string `json:"edenIP"`
	EdenGame string `json:"edenGame"`

	SwitchIP   string `json:"switchIP"`
	SwitchSeen int64  `json:"switchSeen"` // unix ms of last LDN activity

	Session     string `json:"session"`     // human readable
	SessionKind string `json:"sessionKind"` // "", "searching", "playing"
	SessionHost string `json:"sessionHost"`

	ToEden   uint64 `json:"toEden"`
	ToSwitch uint64 `json:"toSwitch"`

	Warning string    `json:"warning"`
	Logs    []LogLine `json:"logs,omitempty"`
}

var (
	stMu   sync.Mutex
	st     = Status{Version: version, RoomMode: "server"}
	logBuf []LogLine
	logID  int
)

func statusUpdate(f func(s *Status)) {
	stMu.Lock()
	f(&st)
	stMu.Unlock()
}

func pushLog(text string) {
	kind := "info"
	l := strings.ToLower(text)
	switch {
	case strings.HasPrefix(l, "aviso") || strings.Contains(l, "error") || strings.Contains(l, "no puedo") || strings.Contains(l, "no pude"):
		kind = "warn"
	case strings.Contains(l, "se une") || strings.Contains(l, "entró") || strings.Contains(l, "creada") || strings.Contains(l, "detectada"):
		kind = "ok"
	}
	stMu.Lock()
	logID++
	logBuf = append(logBuf, LogLine{ID: logID, Time: time.Now().Format("15:04:05"), Text: text, Kind: kind})
	if len(logBuf) > 400 {
		logBuf = logBuf[len(logBuf)-400:]
	}
	stMu.Unlock()
}

// snapshot returns the status plus log lines newer than `after`.
func snapshot(after int) Status {
	stMu.Lock()
	defer stMu.Unlock()
	s := st
	for _, l := range logBuf {
		if l.ID > after {
			s.Logs = append(s.Logs, l)
		}
	}
	return s
}

func setSession(kind, text, host string) {
	statusUpdate(func(s *Status) {
		s.SessionKind, s.Session, s.SessionHost = kind, text, host
	})
}

func touchSwitch(ip IPv4) {
	statusUpdate(func(s *Status) {
		s.SwitchIP = ip.String()
		s.SwitchSeen = time.Now().UnixMilli()
	})
}
