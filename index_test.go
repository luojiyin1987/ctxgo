package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexSearchAndIdempotentReindex(t *testing.T) {
	root := t.TempDir()
	rec, err := execute(context.Background(), root, shell(t, `printf 'first line\nerror: fallback timeout\n成功处理\n'; printf 'panic: second timeout\n' >&2; exit 17`))
	if err != nil { t.Fatal(err) }
	db, err := openIndex(root)
	if err != nil { t.Fatal(err) }
	defer db.Close()
	for i := 0; i < 2; i++ {
		count, err := indexRun(db, root, rec.ID)
		if err != nil || count != 4 { t.Fatalf("index #%d: count=%d err=%v", i, count, err) }
		hits, err := searchIndex(db, "timeout", 20)
		if err != nil || len(hits) != 2 { t.Fatalf("search #%d: %v %v", i, hits, err) }
		streams := map[string]int64{"stderr": 1, "stdout": 2}
		for _, hit := range hits {
			if hit.RunID != rec.ID || hit.Line != streams[hit.Stream] {
				t.Fatalf("unexpected hit: %+v", hit)
			}
		}
	}
	hits, err := searchIndex(db, "fallback timeout", 20)
	if err != nil || len(hits) != 1 || hits[0].Stream != "stdout" {
		t.Fatalf("literal AND search: %v %v", hits, err)
	}
	hits, err = searchIndex(db, "成功处理", 20)
	if err != nil || len(hits) != 1 { t.Fatalf("Unicode search: %v %v", hits, err) }
	var name string
	var exitCode, lines int
	if err := db.QueryRow("SELECT command_json, exit_code, indexed_lines FROM indexed_runs WHERE id=?", rec.ID).Scan(&name, &exitCode, &lines); err != nil || exitCode != 17 || lines != 4 || !strings.Contains(name, "printf") {
		t.Fatalf("metadata: %s %d %d %v", name, exitCode, lines, err)
	}
}

func TestReindexFailurePreservesExistingIndex(t *testing.T) {
	root := t.TempDir()
	rec, err := execute(context.Background(), root, shell(t, "echo 'fatal: still here'"))
	if err != nil { t.Fatal(err) }
	db, err := openIndex(root)
	if err != nil { t.Fatal(err) }
	defer db.Close()
	if _, err := indexRun(db, root, rec.ID); err != nil { t.Fatal(err) }
	if err := os.Remove(filepath.Join(root, rec.ID, "stderr")); err != nil { t.Fatal(err) }
	if _, err := indexRun(db, root, rec.ID); err == nil { t.Fatal("missing raw file should fail") }
	hits, err := searchIndex(db, "still", 10)
	if err != nil || len(hits) != 1 { t.Fatalf("existing index lost: %v %v", hits, err) }
}

func TestLongLineBoundedAndOriginalRecall(t *testing.T) {
	root := t.TempDir()
	id, dir, err := newRunDir(root)
	if err != nil { t.Fatal(err) }
	line := append([]byte("timeout "), bytes.Repeat([]byte("x"), 2<<20)...)
	line = append(line, '\n')
	if err := os.WriteFile(filepath.Join(dir, "stdout"), line, 0600); err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(dir, "stderr"), []byte(""), 0600); err != nil { t.Fatal(err) }
	meta := fmt.Sprintf(`{"id":%q,"command":["large"],"exit_code":1,"stdout_bytes":%d}`, id, len(line))
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte(meta), 0600); err != nil { t.Fatal(err) }
	db, err := openIndex(root)
	if err != nil { t.Fatal(err) }
	defer db.Close()
	if _, err := indexRun(db, root, id); err != nil { t.Fatal(err) }
	hits, err := searchIndex(db, "timeout", 10)
	if err != nil || len(hits) != 1 || len(hits[0].Text) > indexLineBytes {
		t.Fatalf("unbounded hit: %d %v", len(hits), err)
	}
	var got bytes.Buffer
	if err := recall(root, id, "stdout", &got); err != nil || !bytes.Equal(got.Bytes(), line) { t.Fatalf("raw recall changed: %v", err) }
}

func TestQueryIsLiteralAndBounded(t *testing.T) {
	for _, query := range []string{"", "   ", "*", `"`, strings.Repeat("x", 513), strings.Repeat("x ", 21)} {
		if _, err := ftsQuery(query); err == nil { t.Errorf("accepted invalid query %q", query) }
	}
	q, err := ftsQuery(`foo"bar OR admin`)
	if err != nil || q != `"foo""bar" AND "OR" AND "admin"` {
		t.Fatalf("FTS escaping %q: %v", q, err)
	}
	if _, err := searchIndex(&sql.DB{}, "valid", 0); err == nil { t.Fatal("accepted invalid limit") }
}

func TestIndexCLI(t *testing.T) {
	root := t.TempDir()
	t.Setenv(envDataDir, root)
	rec, err := execute(context.Background(), root, shell(t, "printf 'error: found\\n'"))
	if err != nil { t.Fatal(err) }
	var out, errOut bytes.Buffer
	if code := runCLI([]string{"index", rec.ID}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "lines: 1") {
		t.Fatalf("index CLI: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := runCLI([]string{"search", "error"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "stdout:1") {
		t.Fatalf("search CLI: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}
