package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func shell(t *testing.T, script string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	return []string{"sh", "-c", script}
}

func TestExecuteAndRecall(t *testing.T) {
	root := t.TempDir()
	rec, err := execute(context.Background(), root, shell(t, `printf 'hello\n'; printf 'warning\n' >&2; exit 7`))
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExitCode != 7 || rec.StdoutBytes != 6 || rec.StderrBytes != 8 {
		t.Fatalf("unexpected record: %+v", rec)
	}
	for stream, expected := range map[string]string{"stdout": "hello\n", "stderr": "warning\n"} {
		var got bytes.Buffer
		if err := recall(root, rec.ID, stream, &got); err != nil {
			t.Fatal(err)
		}
		if got.String() != expected {
			t.Fatalf("%s = %q, want %q", stream, got.String(), expected)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, rec.ID, "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored runRecord
	if err := json.Unmarshal(data, &stored); err != nil || stored.ID != rec.ID || stored.ExitCode != 7 {
		t.Fatalf("record decode: %+v, %v", stored, err)
	}
}

func TestRunCLIExitCode(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())
	var stdout, stderr bytes.Buffer
	code := runCLI(append([]string{"run", "--"}, shell(t, "exit 23")...), &stdout, &stderr)
	if code != 23 || !strings.Contains(stdout.String(), "exit_code: 23") || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestFinishedProcessKeepsExitStatus(t *testing.T) {
	argv := shell(t, "exit 23")
	err := exec.Command(argv[0], argv[1:]...).Run()
	for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
		code, statusErr := exitCodeFromWait(err, cancellation)
		if statusErr != nil || code != 23 {
			t.Fatalf("code=%d, err=%v, cancellation=%v", code, statusErr, cancellation)
		}
	}
}

func TestExternallySignaledProcessKeepsSignalStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX signals")
	}
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code, statusErr := exitCodeFromWait(cmd.Wait(), nil)
	if statusErr != nil || code != 143 {
		t.Fatalf("code=%d, err=%v", code, statusErr)
	}
}

func TestTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	rec, err := execute(ctx, t.TempDir(), shell(t, "sleep 10"))
	if err != nil || rec.ExitCode != 124 {
		t.Fatalf("record=%+v err=%v", rec, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timed-out command was not stopped promptly")
	}
}

func TestRecallRejectsTraversal(t *testing.T) {
	err := recall(t.TempDir(), "../secret", "stdout", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "invalid run ID") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStartFailureDoesNotLeaveRun(t *testing.T) {
	root := t.TempDir()
	_, err := execute(context.Background(), root, []string{"ctxgo-command-that-does-not-exist-xyz"})
	if err == nil || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("unexpected error: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("leftover directories: %v, %v", entries, err)
	}
}
