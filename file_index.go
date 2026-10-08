package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const maxIndexedFileBytes = 1 << 20

type fileHit struct {
	Path string
	Line int64
	Text string
}

// openFileIndex adds file indexing tables without altering run-output indexes.
func openFileIndex(root string) (*sql.DB, error) {
	db, err := openIndex(root)
	if err != nil {
		return nil, err
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS file_documents (
			id INTEGER PRIMARY KEY,
			path TEXT NOT NULL UNIQUE,
			size_bytes INTEGER NOT NULL,
			modified_ns INTEGER NOT NULL,
			indexed_lines INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS file_lines (
			id INTEGER PRIMARY KEY,
			document_id INTEGER NOT NULL REFERENCES file_documents(id) ON DELETE CASCADE,
			line_no INTEGER NOT NULL,
			content TEXT NOT NULL,
			UNIQUE(document_id, line_no)
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS file_fts USING fts5(
			content, content='file_lines', content_rowid='id', tokenize='unicode61'
		)`,
		`CREATE TRIGGER IF NOT EXISTS file_lines_ai AFTER INSERT ON file_lines BEGIN
			INSERT INTO file_fts(rowid, content) VALUES (new.id, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS file_lines_ad AFTER DELETE ON file_lines BEGIN
			INSERT INTO file_fts(file_fts, rowid, content) VALUES ('delete', old.id, old.content);
		END`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize file index: %w", err)
		}
	}
	return db, nil
}

// indexPath indexes a single regular text file or walks a directory without
// following symlinks. Directory indexing is incremental, file by file.
func indexPath(db *sql.DB, input string) (files int, lines int64, err error) {
	absolute, err := filepath.Abs(input)
	if err != nil {
		return 0, 0, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return 0, 0, err
	}
	if info.Mode().IsRegular() {
		count, err := indexFile(db, absolute)
		if err != nil {
			return 0, 0, err
		}
		return 1, count, nil
	}
	if !info.IsDir() {
		return 0, 0, errors.New("path must be a regular file or directory (symlinks not followed)")
	}
	err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != absolute && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			switch entry.Name() {
			case "node_modules", "vendor", "dist", "build":
				if path != absolute {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !entry.Type().IsRegular() || !indexableFilename(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxIndexedFileBytes {
			return nil
		}
		count, err := indexFile(db, path)
		if errors.Is(err, errBinaryFile) {
			return nil
		}
		if err != nil {
			return err
		}
		files++
		lines += count
		return nil
	})
	return files, lines, err
}

func indexableFilename(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".md", ".txt", ".ts", ".tsx", ".js", ".jsx", ".json",
		".yaml", ".yml", ".py", ".rs", ".c", ".h", ".cpp", ".hpp",
		".sh", ".sql", ".toml", ".html", ".css", ".xml", ".java",
		".vue", ".svelte", ".swift", ".kt":
		return true
	}
	switch name {
	case "README", "LICENSE", "Dockerfile", "Makefile":
		return true
	}
	return false
}

var errBinaryFile = errors.New("binary file cannot be indexed")

// indexFile replaces one document and its FTS entries in a single transaction.
// The source is never changed; any read or write error rolls back the index.
func indexFile(db *sql.DB, path string) (count int64, err error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("not a regular file")
	}
	if info.Size() > maxIndexedFileBytes {
		return 0, fmt.Errorf("file exceeds %d bytes", maxIndexedFileBytes)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var head [4096]byte
	n, err := file.Read(head[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return 0, errBinaryFile
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM file_lines WHERE document_id IN (SELECT id FROM file_documents WHERE path = ?)", absolute); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM file_documents WHERE path = ?", absolute); err != nil {
		return 0, err
	}
	result, err := tx.Exec("INSERT INTO file_documents(path,size_bytes,modified_ns,indexed_lines) VALUES(?,?,?,0)",
		absolute, info.Size(), info.ModTime().UnixNano())
	if err != nil {
		return 0, err
	}
	documentID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	insert, err := tx.Prepare("INSERT INTO file_lines(document_id,line_no,content) VALUES(?,?,?)")
	if err != nil {
		return 0, err
	}
	defer insert.Close()
	reader := bufio.NewReaderSize(file, 32*1024)
	var lineNumber int64
	for {
		line, eof, err := readIndexLine(reader)
		if err != nil {
			return 0, err
		}
		if eof && len(line) == 0 {
			break
		}
		lineNumber++
		text := strings.ToValidUTF8(strings.TrimRight(string(line), "\r\n"), "�")
		text = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) && r != '\t' {
				return ' '
			}
			return r
		}, text)
		if text != "" {
			if _, err := insert.Exec(documentID, lineNumber, text); err != nil {
				return 0, err
			}
			count++
		}
		if eof {
			break
		}
	}
	after, err := os.Stat(absolute)
	if err != nil {
		return 0, err
	}
	if !after.Mode().IsRegular() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return 0, errors.New("file changed while being indexed; retry")
	}
	if _, err := tx.Exec("UPDATE file_documents SET indexed_lines = ? WHERE id = ?", count, documentID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// pruneStaleFiles removes old entries before searching. Modified files must
// be re-indexed explicitly to become searchable again.
func pruneStaleFiles(db *sql.DB) error {
	rows, err := db.Query("SELECT id,path,size_bytes,modified_ns FROM file_documents")
	if err != nil {
		return err
	}
	var stale []int64
	for rows.Next() {
		var id, size, modified int64
		var path string
		if err := rows.Scan(&id, &path, &size, &modified); err != nil {
			rows.Close()
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != size || info.ModTime().UnixNano() != modified {
			stale = append(stale, id)
		}
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil || len(stale) == 0 {
		return readErr
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range stale {
		if _, err := tx.Exec("DELETE FROM file_lines WHERE document_id = ?", id); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM file_documents WHERE id = ?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func searchFiles(db *sql.DB, query string, limit int) ([]fileHit, error) {
	if limit < 1 || limit > maxSearchLimit {
		return nil, fmt.Errorf("limit must be between 1 and %d", maxSearchLimit)
	}
	match, err := ftsQuery(query)
	if err != nil {
		return nil, err
	}
	if err := pruneStaleFiles(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT d.path, l.line_no, l.content
		FROM file_fts JOIN file_lines AS l ON l.id = file_fts.rowid
		JOIN file_documents AS d ON d.id = l.document_id
		WHERE file_fts MATCH ? ORDER BY bm25(file_fts), l.id LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []fileHit
	for rows.Next() {
		var hit fileHit
		if err := rows.Scan(&hit.Path, &hit.Line, &hit.Text); err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}
