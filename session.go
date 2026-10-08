package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxSessionTaskBytes = 512
	maxSessionEventBytes = 4096
	maxSessionResults = 100
)

type sessionRecord struct {
	ID string
	Task string
	Status string
	CreatedAt string
	ClosedAt sql.NullString
}

type sessionEvent struct {
	ID int64
	Kind string
	Text string
	CreatedAt string
}

type sessionSnapshot struct {
	Session sessionRecord
	Events []sessionEvent
	TotalEvents int64
}

func openSessionStore(root string) (*sql.DB, error) {
	db, err := openIndex(root)
	if err != nil {
		return nil, err
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			task TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('active', 'closed')),
			created_at TEXT NOT NULL,
			closed_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS session_events (
			id INTEGER PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			kind TEXT NOT NULL CHECK (kind IN ('decision', 'constraint', 'progress', 'result', 'next')),
			text TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		"CREATE INDEX IF NOT EXISTS session_events_session ON session_events(session_id, id)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize sessions: %w", err)
		}
	}
	return db, nil
}

func validSessionText(value string, maxBytes int) bool {
	return len(value) > 0 && len(value) <= maxBytes &&
		strings.TrimSpace(value) != "" && utf8.ValidString(value)
}

func newSession(db *sql.DB, task string) (string, error) {
	if !validSessionText(task, maxSessionTaskBytes) {
		return "", fmt.Errorf("task must be 1-%d bytes of valid, nonblank UTF-8", maxSessionTaskBytes)
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	_, err := db.Exec(`INSERT INTO sessions(id,task,status,created_at) VALUES(?,?,'active',?)`,
		id, task, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return id, nil
}

func validSessionKind(kind string) bool {
	switch kind {
	case "decision", "constraint", "progress", "result", "next":
		return true
	}
	return false
}

// addSessionEvent is a single conditional write: a closed session cannot
// accept an event, even when another process is closing it concurrently.
func addSessionEvent(db *sql.DB, id, kind, message string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid session ID")
	}
	if !validSessionKind(kind) {
		return errors.New("kind must be decision, constraint, progress, result, or next")
	}
	if !validSessionText(message, maxSessionEventBytes) {
		return fmt.Errorf("event must be 1-%d bytes of valid, nonblank UTF-8", maxSessionEventBytes)
	}
	result, err := db.Exec(`INSERT INTO session_events(session_id,kind,text,created_at)
		SELECT id, ?, ?, ? FROM sessions WHERE id = ? AND status = 'active'`,
		kind, message, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("session does not exist or is closed")
	}
	return nil
}

func closeSession(db *sql.DB, id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid session ID")
	}
	result, err := db.Exec(`UPDATE sessions SET status='closed', closed_at=?
		WHERE id=? AND status='active'`,
		time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("session does not exist or is already closed")
	}
	return nil
}

// loadSession returns the latest events in chronological order and reports
// the total count so older omitted decisions cannot be silently forgotten.
func loadSession(db *sql.DB, id string, limit int) (sessionSnapshot, error) {
	var snapshot sessionSnapshot
	if !validID.MatchString(id) {
		return snapshot, errors.New("invalid session ID")
	}
	if limit < 1 || limit > maxSessionResults {
		return snapshot, fmt.Errorf("limit must be 1-%d", maxSessionResults)
	}
	err := db.QueryRow(`SELECT id,task,status,created_at,closed_at FROM sessions WHERE id=?`, id).
		Scan(&snapshot.Session.ID, &snapshot.Session.Task, &snapshot.Session.Status,
			&snapshot.Session.CreatedAt, &snapshot.Session.ClosedAt)
	if err != nil {
		return snapshot, err
	}
	if err := db.QueryRow("SELECT count(*) FROM session_events WHERE session_id=?", id).
		Scan(&snapshot.TotalEvents); err != nil {
		return snapshot, err
	}
	rows, err := db.Query(`SELECT id,kind,text,created_at FROM session_events
		WHERE session_id=? ORDER BY id DESC LIMIT ?`, id, limit)
	if err != nil {
		return snapshot, err
	}
	defer rows.Close()
	for rows.Next() {
		var event sessionEvent
		if err := rows.Scan(&event.ID, &event.Kind, &event.Text, &event.CreatedAt); err != nil {
			return snapshot, err
		}
		snapshot.Events = append(snapshot.Events, event)
	}
	if err := rows.Err(); err != nil {
		return snapshot, err
	}
	for i, j := 0, len(snapshot.Events)-1; i < j; i, j = i+1, j-1 {
		snapshot.Events[i], snapshot.Events[j] = snapshot.Events[j], snapshot.Events[i]
	}
	return snapshot, nil
}

func listSessions(db *sql.DB, limit int) ([]sessionRecord, error) {
	if limit < 1 || limit > maxSessionResults {
		return nil, fmt.Errorf("limit must be 1-%d", maxSessionResults)
	}
	rows, err := db.Query(`SELECT id,task,status,created_at,closed_at FROM sessions
		ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []sessionRecord
	for rows.Next() {
		var item sessionRecord
		if err := rows.Scan(&item.ID, &item.Task, &item.Status, &item.CreatedAt, &item.ClosedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, item)
	}
	return sessions, rows.Err()
}
