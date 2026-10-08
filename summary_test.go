package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSummaryAndRawRecall(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())
	var stdout, stderr bytes.Buffer
	code := runCLI(append([]string{"run", "--summary-lines", "3", "--"},
		shell(t, `printf 'started\nerror: important detail\ndone\n'; printf 'stderr warning\n' >&2; exit 42`)...), &stdout, &stderr)
	if code != 42 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stdout:2: error: important detail") {
		t.Fatalf("diagnostic not shown: %q", stdout.String())
	}
	id := strings.Fields(strings.SplitN(stdout.String(), "\n", 2)[0])[1]
	var full bytes.Buffer
	if err := recall(os.Getenv(envDataDir), id, "stdout", &full); err != nil {
		t.Fatal(err)
	}
	if full.String() != "started\nerror: important detail\ndone\n" {
		t.Fatalf("raw output changed: %q", full.String())
	}

	var preview bytes.Buffer
	if code := runCLI([]string{"summary", "--lines", "3", id}, &preview, &stderr); code != 0 || !strings.Contains(preview.String(), "error: important detail") {
		t.Fatalf("summary command failed: code=%d output=%q error=%q", code, preview.String(), stderr.String())
	}
}

func TestSummarySelectsEarlyAndLateDiagnostics(t *testing.T) {
	root := t.TempDir()
	rec, err := execute(context.Background(), root, shell(t, `echo 'error: first'; i=0; while [ "$i" -lt 200 ]; do echo "info $i"; i=$((i+1)); done; echo 'fatal: last'`))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := summarizeRun(root, rec.ID, 4, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "error: first") || !strings.Contains(out.String(), "fatal: last") {
		t.Fatalf("missed diagnostic: %q", out.String())
	}
	if lines := strings.Count(out.String(), "\n"); lines > 4 {
		t.Fatalf("unbounded summary (%d lines)", lines)
	}
}

func TestSummaryHugeLineAndControls(t *testing.T) {
	root := t.TempDir()
	id, dir, err := newRunDir(root)
	if err != nil {
		t.Fatal(err)
	}
	content := append([]byte("fatal: "), bytes.Repeat([]byte{'x'}, 2<<20)...)
	content = append(content, '\n', '\x1b', '[', '3', '1', 'm', 'a', '\x00', 'b', 0xff, '\n')
	if err := os.WriteFile(filepath.Join(dir, "stdout"), content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stderr"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte(`{"exit_code":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := summarizeRun(root, id, 4, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() > 1200 || !strings.Contains(out.String(), "fatal:") || !strings.Contains(out.String(), "[line truncated]") {
		t.Fatalf("bad bounded output, len=%d: %q", out.Len(), out.String())
	}
	if bytes.Contains(out.Bytes(), []byte{'\x1b'}) || bytes.Contains(out.Bytes(), []byte{'\x00'}) {
		t.Fatal("terminal control characters leaked into preview")
	}
	var recalled bytes.Buffer
	if err := recall(root, id, "stdout", &recalled); err != nil || !bytes.Equal(content, recalled.Bytes()) {
		t.Fatalf("raw bytes altered or recall failed: %v", err)
	}
}

func TestRunSummaryCanBeDisabled(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())
	var out, errOut bytes.Buffer
	code := runCLI(append([]string{"run", "--summary-lines", "0", "--"}, shell(t, "echo important")...), &out, &errOut)
	if code != 0 || strings.Contains(out.String(), "important") || strings.Contains(out.String(), "summary (") {
		t.Fatalf("unexpected result code=%d out=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestInvalidSummaryArguments(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())
	for _, args := range [][]string{{"run", "--summary-lines", "101", "--", "echo", "x"}, {"summary", "--lines", "-1", "abc"}, {"summary", "../escape"}} {
		var out, errOut bytes.Buffer
		code := runCLI(args, &out, &errOut)
		if code == 0 {
			t.Errorf("expected failure for %v", args)
		}
	}
}
