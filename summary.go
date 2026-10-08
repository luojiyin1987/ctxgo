package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

const (
	defaultSummaryLines = 12
	maxSummaryLines     = 100
	maxLineBytes        = 240
)

// These are candidate diagnostics, not an assertion about the command's status.
var diagnostic = regexp.MustCompile(`(?i)\b(error|errors|failed|failure|fatal|panic|exception|traceback)\b`)

type outputLine struct {
	stream string
	number int64
	text   string
}

type outputSamples struct {
	firstDiagnostics []outputLine
	lastDiagnostics  []outputLine
	tail             []outputLine
}

// scanOutput samples bounded prefixes of lines while consuming the entire file.
// A very long line cannot cause proportional memory allocation.
func scanOutput(path, stream string) (outputSamples, error) {
	var samples outputSamples
	file, err := os.Open(path)
	if err != nil {
		return samples, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 32*1024)
	var number int64
	for {
		preview := make([]byte, 0, maxLineBytes)
		long := false
		endOfFile := false
		for {
			chunk, readErr := reader.ReadSlice('\n')
			if len(preview) < maxLineBytes {
				remaining := maxLineBytes - len(preview)
				if len(chunk) > remaining {
					long = true
					chunkPreview := chunk[:remaining]
					preview = append(preview, chunkPreview...)
				} else {
					preview = append(preview, chunk...)
				}
			} else if len(chunk) > 0 {
				long = true
			}
			switch {
			case readErr == nil:
				// Finished this line.
			case errors.Is(readErr, bufio.ErrBufferFull):
				continue
			case errors.Is(readErr, io.EOF):
				endOfFile = true
				if len(chunk) == 0 && len(preview) == 0 {
					return samples, nil
				}
			default:
				return samples, readErr
			}
			break
		}
		number++
		value := strings.TrimRight(string(preview), "\r\n")
		value = strings.ToValidUTF8(value, "�")
		value = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) && r != '\t' {
				return ' '
			}
			return r
		}, value)
		value = strings.TrimSpace(value)
		if long {
			value += "… [line truncated]"
		}
		entry := outputLine{stream: stream, number: number, text: value}
		samples.tail = retainLast(samples.tail, entry, 6)
		if diagnostic.MatchString(value) {
			if len(samples.firstDiagnostics) < 4 {
				samples.firstDiagnostics = append(samples.firstDiagnostics, entry)
			}
			samples.lastDiagnostics = retainLast(samples.lastDiagnostics, entry, 4)
		}
		if endOfFile {
			return samples, nil
		}
	}
}

func retainLast(lines []outputLine, entry outputLine, capLines int) []outputLine {
	if len(lines) == capLines {
		copy(lines, lines[1:])
		lines[len(lines)-1] = entry
		return lines
	}
	return append(lines, entry)
}

// summarizeRun reads the saved output and emits only a bounded set of candidates.
// stdout/stderr chronology is not reconstructed; use recall for full evidence.
func summarizeRun(root, id string, maxLines int, out io.Writer) error {
	if !validID.MatchString(id) {
		return errors.New("invalid run ID")
	}
	if maxLines < 0 || maxLines > maxSummaryLines {
		return fmt.Errorf("summary lines must be between 0 and %d", maxSummaryLines)
	}
	if maxLines == 0 {
		return nil
	}
	dir := filepath.Join(root, id)
	data, err := os.ReadFile(filepath.Join(dir, "record.json"))
	if err != nil {
		return err
	}
	var rec runRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return err
	}
	stdout, err := scanOutput(filepath.Join(dir, "stdout"), "stdout")
	if err != nil {
		return err
	}
	stderr, err := scanOutput(filepath.Join(dir, "stderr"), "stderr")
	if err != nil {
		return err
	}

	candidates := make([][]outputLine, 0, 6)
	candidates = append(candidates, stderr.firstDiagnostics, stdout.firstDiagnostics,
		stderr.lastDiagnostics, stdout.lastDiagnostics)
	if rec.ExitCode == 0 {
		candidates = append(candidates, stdout.tail, stderr.tail)
	} else {
		candidates = append(candidates, stderr.tail, stdout.tail)
	}
	seen := make(map[outputLine]bool, maxLines)
	selected := make([]outputLine, 0, maxLines)
	for _, group := range candidates {
		for _, line := range group {
			if len(selected) == maxLines {
				break
			}
			if seen[line] || line.text == "" {
				continue
			}
			seen[line] = true
			selected = append(selected, line)
		}
	}
	if len(selected) == 0 {
		_, err := fmt.Fprintln(out, "  (no nonempty output)")
		return err
	}
	for _, line := range selected {
		if _, err := fmt.Fprintf(out, "  %s:%d: %s\n", line.stream, line.number, line.text); err != nil {
			return err
		}
	}
	return nil
}
