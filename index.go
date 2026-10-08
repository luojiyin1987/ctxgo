package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	_ "modernc.org/sqlite"
)

const (
	indexLineBytes = 4096
	maxSearchLimit = 100
)

type searchHit struct {
	RunID  string
	Stream string
	Line   int64
	Text   string
}

// openIndex opens the local FTS5 database; one connection keeps PRAGMA settings
// scoped to the driver connection rather than assuming they apply globally.
func openIndex(root string) (*sql.DB, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "index.sqlite")
	// SQLite creates this with its own permissions; ensure the database itself
	// is never world-readable, including when the directory is user-supplied.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		`CREATE TABLE IF NOT EXISTS indexed_runs (
			id TEXT PRIMARY KEY,
			command_json TEXT NOT NULL,
			exit_code INTEGER NOT NULL,
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL,
			stdout_bytes INTEGER NOT NULL,
			stderr_bytes INTEGER NOT NULL,
			indexed_lines INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS output_lines (
			id INTEGER PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES indexed_runs(id) ON DELETE CASCADE,
			stream TEXT NOT NULL CHECK(stream IN ('stdout','stderr')),
			line_no INTEGER NOT NULL,
			content TEXT NOT NULL,
			UNIQUE(run_id, stream, line_no)
		)`,
		"CREATE INDEX IF NOT EXISTS output_lines_run ON output_lines(run_id)",
		`CREATE VIRTUAL TABLE IF NOT EXISTS output_fts USING fts5(
			content, content='output_lines', content_rowid='id', tokenize='unicode61'
		)`,
		`CREATE TRIGGER IF NOT EXISTS output_lines_ai AFTER INSERT ON output_lines BEGIN
			INSERT INTO output_fts(rowid, content) VALUES (new.id, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS output_lines_ad AFTER DELETE ON output_lines BEGIN
			INSERT INTO output_fts(output_fts, rowid, content) VALUES ('delete', old.id, old.content);
		END`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize index: %w", err)
		}
	}
	return db, nil
}

// indexRun atomically replaces an indexed run using the persisted run record
// and output files as the source of truth. Re-indexing never duplicates rows.
func indexRun(db *sql.DB, root, id string) (count int64, err error) {
	if !validID.MatchString(id) {
		return 0, errors.New("invalid run ID")
	}
	dir := filepath.Join(root, id)
	payload, err := os.ReadFile(filepath.Join(dir, "record.json"))
	if err != nil {
		return 0, fmt.Errorf("read run: %w", err)
	}
	var record runRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return 0, fmt.Errorf("decode run: %w", err)
	}
	if record.ID != id {
		return 0, errors.New("run record ID does not match directory")
	}
	command, err := json.Marshal(record.Command)
	if err != nil {
		return 0, err
	}
	// Open inputs before changing the index. Missing raw output must never
	// wipe a previously good index of the same run.
	files := make([]*os.File, 0, 2)
	for _, stream := range []string{"stdout", "stderr"} {
		file, err := os.Open(filepath.Join(dir, stream))
		if err != nil {
			for _, f := range files {
				f.Close()
			}
			return 0, err
		}
		files = append(files, file)
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM output_lines WHERE run_id = ?", id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM indexed_runs WHERE id = ?", id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO indexed_runs
		(id, command_json, exit_code, started_at, finished_at, stdout_bytes, stderr_bytes, indexed_lines)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)`, id, string(command), record.ExitCode,
		record.StartedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
		record.FinishedAt.Format("2006-01-02T15:04:05.999999999Z07:00"), record.StdoutBytes, record.StderrBytes); err != nil {
		return 0, err
	}
	insert, err := tx.Prepare("INSERT INTO output_lines(run_id,stream,line_no,content) VALUES(?,?,?,?)")
	if err != nil {
		return 0, err
	}
	defer insert.Close()
	for i, stream := range []string{"stdout", "stderr"} {
		reader := bufio.NewReaderSize(files[i], 32*1024)
		var lineNo int64
		for {
			line, eof, err := readIndexLine(reader)
			if err != nil {
				return 0, err
			}
			if eof && len(line) == 0 {
				break
			}
			lineNo++
			text := strings.ToValidUTF8(strings.TrimRight(string(line), "\r\n"), "�")
			text = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) && r != '\t' {
					return ' '
				}
				return r
			}, text)
			if text != "" {
				if _, err := insert.Exec(id, stream, lineNo, text); err != nil {
					return 0, fmt.Errorf("index %s:%d: %w", stream, lineNo, err)
				}
				count++
			}
			if eof {
				break
			}
		}
	}
	if _, err := tx.Exec("UPDATE indexed_runs SET indexed_lines = ? WHERE id = ?", count, id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// readIndexLine consumes a full physical line but retains only a fixed-size
// prefix for the index. The source file remains available for exact recall.
func readIndexLine(reader *bufio.Reader) (line []byte, eof bool, err error) {
	line = make([]byte, 0, indexLineBytes)
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if len(line) < indexLineBytes {
			remaining := indexLineBytes - len(line)
			if len(chunk) > remaining {
				chunk = chunk[:remaining]
			}
			line = append(line, chunk...)
		}
		switch {
		case readErr == nil:
			return line, false, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			return line, true, nil
		default:
			return nil, false, readErr
		}
	}
}

// ftsQuery turns user text into literal AND terms, not executable FTS syntax.
func ftsQuery(query string) (string, error) {
	fields := strings.Fields(query)
	if len(fields) == 0 || len(fields) > 20 || len(query) > 512 {
		return "", errors.New("search requires 1-20 terms (maximum 512 bytes)")
	}
	terms := make([]string, 0, len(fields))
	for _, term := range fields {
		if !strings.ContainsFunc(term, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			return "", fmt.Errorf("invalid search term: %q", term)
		}
		terms = append(terms, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " AND "), nil
}

func searchIndex(db *sql.DB, query string, limit int) ([]searchHit, error) {
	if limit < 1 || limit > maxSearchLimit {
		return nil, fmt.Errorf("limit must be between 1 and %d", maxSearchLimit)
	}
	match, err := ftsQuery(query)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT l.run_id, l.stream, l.line_no, l.content
		FROM output_fts JOIN output_lines AS l ON l.id = output_fts.rowid
		WHERE output_fts MATCH ? ORDER BY bm25(output_fts), l.id LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []searchHit
	for rows.Next() {
		var hit searchHit
		if err := rows.Scan(&hit.RunID, &hit.Stream, &hit.Line, &hit.Text); err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}
