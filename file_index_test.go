package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestFilesIndexAndSearch(t *testing.T) {
	root := t.TempDir()
	project := t.TempDir()
	mainFile := writeFixture(t, filepath.Join(project, "main.go"), "package main\n// uncommonkeyword here\n")
	nested := writeFixture(t, filepath.Join(project, "internal", "note.md"), "first\nsecond unusualphrase\n")
	writeFixture(t, filepath.Join(project, ".git", "private.txt"), "uncommonkeyword\n")
	writeFixture(t, filepath.Join(project, "node_modules", "dep.js"), "uncommonkeyword\n")
	writeFixture(t, filepath.Join(project, "binary.dat"), "uncommonkeyword\n")
	writeFixture(t, filepath.Join(project, "large.go"), strings.Repeat("x", maxIndexedFileBytes+1))
	db, err := openFileIndex(root)
	if err != nil { t.Fatal(err) }
	defer db.Close()
	for attempt := 0; attempt < 2; attempt++ {
		files, lines, err := indexPath(db, project)
		if err != nil || files != 2 || lines != 4 {
			t.Fatalf("attempt %d: files=%d lines=%d err=%v", attempt, files, lines, err)
		}
		hits, err := searchFiles(db, "uncommonkeyword", 20)
		if err != nil || len(hits) != 1 || hits[0].Path != mainFile || hits[0].Line != 2 {
			t.Fatalf("file hits: %+v err=%v", hits, err)
		}
		hits, err = searchFiles(db, "unusualphrase", 20)
		if err != nil || len(hits) != 1 || hits[0].Path != nested || hits[0].Line != 2 {
			t.Fatalf("nested hits: %+v err=%v", hits, err)
		}
	}
	var documents int
	if err := db.QueryRow("SELECT count(*) FROM file_documents").Scan(&documents); err != nil || documents != 2 {
		t.Fatalf("documents=%d err=%v", documents, err)
	}
}

func TestFilesChangedAndDeletedInvalidate(t *testing.T) {
	db, err := openFileIndex(t.TempDir())
	if err != nil { t.Fatal(err) }
	defer db.Close()
	source := writeFixture(t, filepath.Join(t.TempDir(), "notes.md"), "stalephrase old\n")
	if _, _, err := indexPath(db, source); err != nil { t.Fatal(err) }
	writeFixture(t, source, "freshphrase updated document\n")
	old, err := searchFiles(db, "stalephrase", 20)
	if err != nil || len(old) != 0 { t.Fatalf("stale results: %v %v", old, err) }
	newHits, err := searchFiles(db, "freshphrase", 20)
	if err != nil || len(newHits) != 0 { t.Fatalf("unindexed content visible: %v %v", newHits, err) }
	if _, _, err := indexPath(db, source); err != nil { t.Fatal(err) }
	newHits, err = searchFiles(db, "freshphrase", 20)
	if err != nil || len(newHits) != 1 { t.Fatalf("reindex: %v %v", newHits, err) }
	if err := os.Remove(source); err != nil { t.Fatal(err) }
	newHits, err = searchFiles(db, "freshphrase", 20)
	if err != nil || len(newHits) != 0 { t.Fatalf("deleted file still indexed: %v %v", newHits, err) }
}

func TestFilesRejectBinarySymlinkAndOversize(t *testing.T) {
	db, err := openFileIndex(t.TempDir())
	if err != nil { t.Fatal(err) }
	defer db.Close()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"binary.txt": "abc\x00def",
		"huge.txt": strings.Repeat("x", maxIndexedFileBytes+1),
	} {
		path := writeFixture(t, filepath.Join(dir, name), content)
		if _, _, err := indexPath(db, path); err == nil {
			t.Fatalf("expected indexing error for %s", name)
		}
	}
	if runtime.GOOS != "windows" {
		path := writeFixture(t, filepath.Join(dir, "target.go"), "secretword\n")
		link := filepath.Join(dir, "alias.go")
		if err := os.Symlink(path, link); err != nil { t.Fatal(err) }
		if _, _, err := indexPath(db, link); err == nil { t.Fatal("followed explicit symlink") }
		if files, _, err := indexPath(db, dir); err != nil || files != 1 {
			t.Fatalf("directory symlink skip: files=%d err=%v", files, err)
		}
	}
}

func TestFilesCoexistWithRunSearch(t *testing.T) {
	root := t.TempDir()
	rec, err := execute(context.Background(), root, shell(t, "echo 'runneronlyterm'"))
	if err != nil { t.Fatal(err) }
	db, err := openFileIndex(root)
	if err != nil { t.Fatal(err) }
	defer db.Close()
	if _, err := indexRun(db, root, rec.ID); err != nil { t.Fatal(err) }
	file := writeFixture(t, filepath.Join(t.TempDir(), "code.go"), "// fileonlyterm\n")
	if _, _, err := indexPath(db, file); err != nil { t.Fatal(err) }
	runHits, err := searchIndex(db, "runneronlyterm", 10)
	if err != nil || len(runHits) != 1 { t.Fatalf("run search: %v %v", runHits, err) }
	fileHits, err := searchFiles(db, "fileonlyterm", 10)
	if err != nil || len(fileHits) != 1 { t.Fatalf("file search: %v %v", fileHits, err) }
	fileHits, err = searchFiles(db, "runneronlyterm", 10)
	if err != nil || len(fileHits) != 0 { t.Fatalf("search polluted by runs: %v %v", fileHits, err) }
}

func TestFilesCLI(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	t.Setenv(envDataDir, root)
	source := writeFixture(t, filepath.Join(dir, "file.md"), "cliuniquetoken\n")
	var out, errors bytes.Buffer
	if code := runCLI([]string{"files", "index", source}, &out, &errors); code != 0 || !strings.Contains(out.String(), "files: 1") {
		t.Fatalf("index CLI code=%d out=%q err=%q", code, out.String(), errors.String())
	}
	out.Reset()
	errors.Reset()
	if code := runCLI([]string{"files", "search", "cliuniquetoken"}, &out, &errors); code != 0 || !strings.Contains(out.String(), strconv.Quote(source)+":1") {
		t.Fatalf("search CLI code=%d out=%q err=%q", code, out.String(), errors.String())
	}
	for _, args := range [][]string{
		{"files", "index"},
		{"files", "search"},
		{"files", "search", "--limit", "0", "word"},
		{"files", "unknown", "word"},
	} {
		out.Reset()
		errors.Reset()
		if code := runCLI(args, &out, &errors); code == 0 {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
}
