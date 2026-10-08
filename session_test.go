package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestSessionLifecycleAndPersistence(t *testing.T) {
	root := t.TempDir()
	db, err := openSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newSession(db, "Fix flaky cancellation and preserve exit codes")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		kind string
		text string
	}{
		{"constraint", "Do not change the public API"},
		{"decision", "Use context cancellation boundaries"},
		{"progress", "Finished focused tests"},
		{"next", "Run full CI before merge"},
		{"result", "Local race detector passed"},
	} {
		if err := addSessionEvent(db, id, item.kind, item.text); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := loadSession(db, id, 3)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Session.Task != "Fix flaky cancellation and preserve exit codes" ||
		snapshot.Session.Status != "active" || snapshot.TotalEvents != 5 ||
		len(snapshot.Events) != 3 || snapshot.Events[0].Kind != "progress" ||
		snapshot.Events[2].Kind != "result" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Events[0].ID >= snapshot.Events[1].ID ||
		snapshot.Events[1].ID >= snapshot.Events[2].ID {
		t.Fatalf("events not chronological: %+v", snapshot.Events)
	}
	if err := closeSession(db, id); err != nil {
		t.Fatal(err)
	}
	if err := addSessionEvent(db, id, "progress", "not allowed"); err == nil {
		t.Fatal("closed session accepted new event")
	}
	if err := closeSession(db, id); err == nil {
		t.Fatal("closed session was closed twice")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSessionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot, err = loadSession(reopened, id, 100)
	if err != nil || snapshot.Session.Status != "closed" ||
		!snapshot.Session.ClosedAt.Valid || len(snapshot.Events) != 5 {
		t.Fatalf("reloaded snapshot=%+v err=%v", snapshot, err)
	}
	sessions, err := listSessions(reopened, 20)
	if err != nil || len(sessions) != 1 || sessions[0].ID != id {
		t.Fatalf("list=%+v err=%v", sessions, err)
	}
}

func TestSessionRejectsInvalidInputs(t *testing.T) {
	db, err := openSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, task := range []string{"", "  ", strings.Repeat("x", maxSessionTaskBytes+1), string([]byte{0xff})} {
		if _, err := newSession(db, task); err == nil {
			t.Errorf("invalid task accepted: %q", task)
		}
	}
	id, err := newSession(db, "Valid task")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []struct {
		id string
		kind string
		text string
	}{
		{"../escape", "decision", "bad ID"},
		{strings.Repeat("a", 24), "decision", "missing ID"},
		{id, "unknown", "bad kind"},
		{id, "decision", ""},
		{id, "decision", strings.Repeat("x", maxSessionEventBytes+1)},
		{id, "decision", string([]byte{0xff})},
	} {
		if err := addSessionEvent(db, candidate.id, candidate.kind, candidate.text); err == nil {
			t.Errorf("invalid event accepted: %+v", candidate)
		}
	}
	if err := closeSession(db, "../escape"); err == nil {
		t.Fatal("invalid close ID accepted")
	}
	if _, err := loadSession(db, id, 0); err == nil {
		t.Fatal("invalid show limit accepted")
	}
	if _, err := listSessions(db, 101); err == nil {
		t.Fatal("invalid list limit accepted")
	}
	if _, err := loadSession(db, strings.Repeat("b", 24), 20); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing session error: %v", err)
	}
}

func TestSessionConcurrentEvents(t *testing.T) {
	db, err := openSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := newSession(db, "Concurrent event storage")
	if err != nil {
		t.Fatal(err)
	}
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			errs <- addSessionEvent(db, id, "progress", fmt.Sprintf("worker %d", n))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := loadSession(db, id, 20)
	if err != nil || snapshot.TotalEvents != writers || len(snapshot.Events) != writers {
		t.Fatalf("unexpected concurrent events: %v %+v", err, snapshot)
	}
}

func TestSessionCLI(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())
	var out, errOut bytes.Buffer
	run := func(args ...string) (int, string, string) {
		out.Reset()
		errOut.Reset()
		code := runCLI(args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	code, response, stderr := run("session", "start", "Fix", "CLI", "logic")
	if code != 0 || !strings.HasPrefix(response, "session_id: ") {
		t.Fatalf("start: code=%d out=%q err=%q", code, response, stderr)
	}
	id := strings.TrimSpace(strings.TrimPrefix(response, "session_id: "))
	if len(id) != 24 {
		t.Fatalf("bad session ID: %q", id)
	}
	for _, args := range [][]string{
		{"session", "add", "--kind", "constraint", id, "Avoid changing public API"},
		{"session", "add", "--kind", "decision", id, "Keep SQLite storage"},
		{"session", "add", "--kind", "next", id, "Run CI"},
	} {
		if code, _, err := run(args...); code != 0 {
			t.Fatalf("add %v: %s", args, err)
		}
	}
	code, response, stderr = run("session", "show", "--limit", "2", id)
	if code != 0 || !strings.Contains(response, "events: 2 of 3") ||
		!strings.Contains(response, "older events omitted") ||
		!strings.Contains(response, "Keep SQLite storage") ||
		strings.Contains(response, "Avoid changing public API") {
		t.Fatalf("show: code=%d out=%q err=%q", code, response, stderr)
	}
	code, response, stderr = run("session", "list")
	if code != 0 || !strings.Contains(response, id) {
		t.Fatalf("list: code=%d out=%q err=%q", code, response, stderr)
	}
	if code, _, stderr := run("session", "close", id); code != 0 {
		t.Fatalf("close: %s", stderr)
	}
	if code, _, _ := run("session", "add", "--kind", "progress", id, "late"); code == 0 {
		t.Fatal("CLI accepted append to closed session")
	}
	for _, args := range [][]string{
		{"session"},
		{"session", "start"},
		{"session", "show", id, "unexpected"},
		{"session", "add", "--kind", "invalid", id, "text"},
		{"session", "list", "--limit", "0"},
		{"session", "close", "invalid"},
	} {
		if code, _, _ := run(args...); code == 0 {
			t.Errorf("accepted invalid CLI args: %v", args)
		}
	}
}
