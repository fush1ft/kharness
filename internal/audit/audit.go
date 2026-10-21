// Package audit records every question, tool call, approval decision and incident in SQLite.
package audit

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Log struct {
	db *sql.DB
}

// Open opens (and migrates) the log. An empty path gives a no-op log.
func Open(path string) (*Log, error) {
	if path == "" {
		return &Log{}, nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS audit (
		id      INTEGER PRIMARY KEY,
		ts      TEXT NOT NULL,
		session TEXT NOT NULL,
		kind    TEXT NOT NULL,
		tool    TEXT NOT NULL DEFAULT '',
		risk    TEXT NOT NULL DEFAULT '',
		args    TEXT NOT NULL DEFAULT '',
		ok      INTEGER NOT NULL DEFAULT 1,
		actor   TEXT NOT NULL DEFAULT '',
		detail  TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("audit: migrate: %w", err)
	}
	return &Log{db: db}, nil
}

func (l *Log) Close() error {
	if l.db == nil {
		return nil
	}
	return l.db.Close()
}

type Entry struct {
	Time    time.Time
	Session string
	Kind    string // question | tool | approval | incident
	Tool    string
	Risk    string
	Args    string
	OK      bool
	Actor   string
	Detail  string
}

func (l *Log) Add(e Entry) {
	if l.db == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	ok := 0
	if e.OK {
		ok = 1
	}
	_, _ = l.db.Exec(`INSERT INTO audit (ts, session, kind, tool, risk, args, ok, actor, detail) VALUES (?,?,?,?,?,?,?,?,?)`,
		e.Time.UTC().Format(time.RFC3339), e.Session, e.Kind, e.Tool, e.Risk, trim(e.Args, 4000), ok, e.Actor, trim(e.Detail, 4000))
}

// Recent returns the last n entries, oldest first. With changesOnly, only approvals and non-read tool calls.
func (l *Log) Recent(n int, changesOnly bool) ([]Entry, error) {
	if l.db == nil {
		return nil, nil
	}
	where := ""
	if changesOnly {
		where = `WHERE kind = 'approval' OR (kind = 'tool' AND risk != 'read')`
	}
	rows, err := l.db.Query(`SELECT ts, session, kind, tool, risk, args, ok, actor, detail FROM audit `+where+` ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var ts string
		var ok int
		if err := rows.Scan(&ts, &e.Session, &e.Kind, &e.Tool, &e.Risk, &e.Args, &ok, &e.Actor, &e.Detail); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339, ts)
		e.OK = ok == 1
		out = append(out, e)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// Format renders entries as text lines.
func Format(es []Entry) string {
	if len(es) == 0 {
		return "(audit log is empty)"
	}
	var b strings.Builder
	for _, e := range es {
		mark := "✓"
		if !e.OK {
			mark = "✗"
		}
		line := fmt.Sprintf("%s %s %-9s %-8s %s", e.Time.Local().Format("01-02 15:04:05"), mark, e.Kind, e.Session, e.Tool)
		if e.Actor != "" {
			line += " by " + e.Actor
		}
		if d := oneLine(e.Detail, 100); d != "" {
			line += " — " + d
		} else if a := oneLine(e.Args, 100); a != "" {
			line += " " + a
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func oneLine(s string, n int) string {
	return trim(strings.Join(strings.Fields(s), " "), n)
}
