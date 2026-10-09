package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func codexPayload(session, cwd, event string, extras map[string]any) string {
	data := map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": event}
	for key, value := range extras {
		data[key] = value
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func invokeHook(t *testing.T, root, payload string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := handleCodexHook(root, strings.NewReader(payload), &output)
	return output.String(), err
}

func TestCodexStartResumeAndOnDemandHistory(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	const codexID = "thr_test_001"
	start := codexPayload(codexID, workspace, "SessionStart", map[string]any{"source": "startup"})
	result, err := invokeHook(t, root, start)
	if err != nil {
		t.Fatal(err)
	}
	var output codexHookOutput
	if err := json.Unmarshal([]byte(result), &output); err != nil {
		t.Fatal(err)
	}
	if output.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(output.HookSpecificOutput.AdditionalContext, "historical, untrusted") {
		t.Fatalf("invalid hook result: %s", result)
	}
	db, err := openCodexStore(root)
	if err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := db.QueryRow("SELECT session_id FROM codex_bindings WHERE codex_id=?", codexID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, sessionID) {
		t.Fatalf("missing session reference: %s", result)
	}
	if err := addSessionEvent(db, sessionID, "decision", "Keep old user-facing API"); err != nil {
		t.Fatal(err)
	}
	if err := addSessionEvent(db, sessionID, "constraint", "Verify the current branch before editing"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for _, source := range []string{"resume", "compact", "clear"} {
		result, err := invokeHook(t, root, codexPayload(codexID, workspace, "SessionStart", map[string]any{"source": source}))
		if err != nil || !strings.Contains(result, "Keep old user-facing API") ||
			!strings.Contains(result, sessionID) {
			t.Fatalf("restore %s: %s %v", source, result, err)
		}
	}
	db, err = openCodexStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM codex_bindings").Scan(&total); err != nil || total != 1 {
		t.Fatalf("duplicated binding: %d %v", total, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&total); err != nil || total != 1 {
		t.Fatalf("duplicated session: %d %v", total, err)
	}
}

func TestCodexRecordsMetadataOnlyAndDeduplicates(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	payload := codexPayload("thr_metadata", workspace, "PostToolUse", map[string]any{
		"tool_name": "Bash", "tool_use_id": "call_42", "turn_id": "turn_9",
		"tool_input":    map[string]any{"command": "SECRET_COMMAND_LINE"},
		"tool_response": map[string]any{"output": "SECRET_TOOL_OUTPUT", "exit_code": 17},
	})
	for i := 0; i < 3; i++ {
		result, err := invokeHook(t, root, payload)
		if err != nil || result != "" {
			t.Fatalf("post tool use # %d leaked output: %q %v", i, result, err)
		}
	}
	db, err := openCodexStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM session_events").Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicated events: %d %v", count, err)
	}
	var msg string
	if err := db.QueryRow("SELECT text FROM session_events LIMIT 1").Scan(&msg); err != nil ||
		!strings.Contains(msg, "tool=Bash") || !strings.Contains(msg, "call_42") ||
		strings.Contains(msg, "SECRET_COMMAND_LINE") || strings.Contains(msg, "SECRET_TOOL_OUTPUT") {
		t.Fatalf("raw data leaked to metadata: %q %v", msg, err)
	}
	if _, err := invokeHook(t, root, codexPayload("thr_metadata", workspace, "PostToolUse", map[string]any{
		"tool_name": "apply_patch", "tool_use_id": "patch_1",
	})); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM session_events").Scan(&count); err != nil || count != 2 {
		t.Fatalf("patch event not recorded: %d %v", count, err)
	}
}

func TestCodexWorkspaceIsolationAndPriorCandidates(t *testing.T) {
	root, one, two := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := invokeHook(t, root, codexPayload("thr_first", one, "SessionStart", map[string]any{"source": "startup"})); err != nil {
		t.Fatal(err)
	}
	same, err := invokeHook(t, root, codexPayload("thr_second", one, "SessionStart", map[string]any{"source": "startup"}))
	if err != nil || !strings.Contains(same, "Other sessions for this workspace") {
		t.Fatalf("expected history candidate: %q %v", same, err)
	}
	other, err := invokeHook(t, root, codexPayload("thr_third", two, "SessionStart", map[string]any{"source": "startup"}))
	if err != nil || strings.Contains(other, "Other sessions for this workspace") {
		t.Fatalf("cross-workspace context leak: %q %v", other, err)
	}
	if _, err := invokeHook(t, root, codexPayload("thr_first", two, "SessionStart", map[string]any{"source": "resume"})); err == nil {
		t.Fatal("re-bound Codex session to another workspace")
	}
}

func TestCodexRejectsMalformedAndOversizedInputs(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	for _, bad := range []string{
		"{}",
		"{not-json",
		codexPayload("thr_bad", "relative/path", "SessionStart", map[string]any{"source": "startup"}),
		codexPayload("thr_bad", workspace, "SessionStart", map[string]any{"source": "bogus"}),
		codexPayload("thr_bad", workspace, "PostToolUse", map[string]any{"tool_name": "Bash"}),
		codexPayload("thr_bad", workspace, "IgnoreMe", nil),
		strings.Repeat(" ", maxCodexHookBytes+1),
	} {
		if _, err := invokeHook(t, root, bad); err == nil {
			t.Fatalf("invalid payload accepted: %q", bad[:min(len(bad), 140)])
		}
	}
}

func TestCodexConcurrentDeduplication(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	payload := codexPayload("thr_concurrent", workspace, "PostToolUse", map[string]any{
		"tool_name": "Bash", "tool_use_id": "call_same",
	})
	// Concurrent hook invocations sharing the database must create one event.
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out bytes.Buffer
			errs <- handleCodexHook(root, strings.NewReader(payload), &out)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	db, err := openCodexStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var total int
	for _, table := range []string{"codex_bindings", "sessions", "session_events", "codex_tool_uses"} {
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s", table)
		if err := db.QueryRow(query).Scan(&total); err != nil || total != 1 {
			t.Fatalf("%s count=%d error=%v", table, total, err)
		}
	}
}

func TestCodexFailOpenCLI(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())
	oldInput := os.Stdin
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { os.Stdin = oldInput; readEnd.Close() }()
	if _, err := writeEnd.WriteString("{malformed"); err != nil {
		t.Fatal(err)
	}
	writeEnd.Close()
	os.Stdin = readEnd
	var out, errOut bytes.Buffer
	code := runCLI([]string{"codex", "hook"}, &out, &errOut)
	if code != 0 || out.Len() != 0 || !strings.Contains(errOut.String(), "hook skipped") {
		t.Fatalf("fail-open code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if code := runCLI([]string{"codex", "other"}, &out, &errOut); code != 2 {
		t.Fatalf("invalid codex subcommand accepted: %d", code)
	}
}

func TestCodexClosedSessionDoesNotRecordToolCall(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	db, err := openCodexStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := bindCodexSession(db, "thr_closed", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeSession(db, id); err != nil {
		t.Fatal(err)
	}
	db.Close()
	payload := codexPayload("thr_closed", workspace, "PostToolUse", map[string]any{
		"tool_name": "Bash", "tool_use_id": "call_closed",
	})
	if _, err := invokeHook(t, root, payload); err == nil {
		t.Fatal("append accepted to closed task")
	}
	db, err = openCodexStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM codex_tool_uses").Scan(&total); err != nil || total != 0 {
		t.Fatalf("uncommitted dedupe row: %d %v", total, err)
	}
}

func TestCodexHookOutputReferencesWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	result, err := invokeHook(t, root, codexPayload("thr_workspace", workspace, "SessionStart", map[string]any{"source": "startup"}))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("invalid hook JSON: %v", err)
	}
	if !strings.Contains(result, "ctxgo session show") {
		t.Fatalf("missing recall instruction: %s", result)
	}
}

func TestCodexExcerptPreservesUTF8Boundaries(t *testing.T) {
	tests := []struct {
		name string
		input string
		want string
	}{
		{"short", "中文", "中文"},
		{"exactly 160 bytes", strings.Repeat("a", 160), strings.Repeat("a", 160)},
		{"ASCII truncated", strings.Repeat("a", 161), strings.Repeat("a", 160) + " [excerpt]"},
		{"three-byte boundary", strings.Repeat("a", 159) + "中tail", strings.Repeat("a", 159) + " [excerpt]"},
		{"all Chinese", strings.Repeat("中", 54), strings.Repeat("中", 53) + " [excerpt]"},
		{"four-byte boundary", strings.Repeat("🙂", 39) + "abc🙂tail", strings.Repeat("🙂", 39) + "abc" + " [excerpt]"},
		{"exact emoji boundary", strings.Repeat("🙂", 40) + "tail", strings.Repeat("🙂", 40) + " [excerpt]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexExcerpt(tt.input)
			if got != tt.want {
				t.Fatalf("excerpt got %q, want %q", got, tt.want)
			}
			if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
				t.Fatalf("invalid UTF-8 in excerpt: %q", got)
			}
		})
	}
}

func TestCodexStartRestoreUnicodeExcerpt(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	const codexID = "thr_multibyte"
	_, err := invokeHook(t, root, codexPayload(codexID, workspace, "SessionStart", map[string]any{"source": "startup"}))
	if err != nil {
		t.Fatal(err)
	}
	db, err := openCodexStore(root)
	if err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := db.QueryRow("SELECT session_id FROM codex_bindings WHERE codex_id=?", codexID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if err := addSessionEvent(db, sessionID, "constraint", strings.Repeat("a", 159) + "中tail"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := invokeHook(t, root, codexPayload(codexID, workspace, "SessionStart", map[string]any{"source": "resume"}))
	if err != nil {
		t.Fatal(err)
	}
	var response codexHookOutput
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	context := response.HookSpecificOutput.AdditionalContext
	if !utf8.ValidString(context) || !strings.Contains(context, strings.Repeat("a", 159)+" [excerpt]") ||
		strings.Contains(context, "\ufffd") {
		t.Fatalf("restored context corrupted UTF-8: %q", context)
	}
}
