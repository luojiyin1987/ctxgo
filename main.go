package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const envDataDir = "CTXGO_DATA_DIR"

var validID = regexp.MustCompile(`^[0-9a-f]{24}$`)

type runRecord struct {
	ID          string    `json:"id"`
	Command     []string  `json:"command"`
	ExitCode    int       `json:"exit_code"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	StdoutBytes int64     `json:"stdout_bytes"`
	StderrBytes int64     `json:"stderr_bytes"`
}

func dataDir() (string, error) {
	if dir := os.Getenv(envDataDir); dir != "" {
		return dir, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "ctxgo", "runs"), nil
}

func newRunDir(root string) (string, string, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", "", err
	}
	for range 3 {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", "", err
		}
		id := hex.EncodeToString(b[:])
		path := filepath.Join(root, id)
		if err := os.Mkdir(path, 0700); err == nil {
			return id, path, nil
		} else if !os.IsExist(err) {
			return "", "", err
		}
	}
	return "", "", errors.New("could not allocate unique run ID")
}

func execute(ctx context.Context, root string, argv []string) (rec runRecord, err error) {
	if len(argv) == 0 {
		return rec, errors.New("missing command")
	}
	id, dir, err := newRunDir(root)
	if err != nil {
		return rec, err
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(dir)
		}
	}()

	stdout, err := os.OpenFile(filepath.Join(dir, "stdout"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return rec, err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(dir, "stderr"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return rec, err
	}
	defer stderr.Close()

	rec = runRecord{ID: id, Command: append([]string(nil), argv...), StartedAt: time.Now().UTC()}
	cmd := exec.Command(argv[0], argv[1:]...)
	configureProcessGroup(cmd)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return rec, fmt.Errorf("start %q: %w", argv[0], err)
	}

	done := make(chan struct{})
	monitorDone := make(chan struct{})
	var cancellationErr error
	go func() {
		defer close(monitorDone)
		select {
		case <-ctx.Done():
			// The child might have completed while cancellation was queued.
			select {
			case <-done:
				return
			default:
			}
			cancellationErr = ctx.Err()
			killProcessGroup(cmd)
		case <-done:
		}
	}()
	waitErr := cmd.Wait()
	close(done)
	<-monitorDone
	rec.FinishedAt = time.Now().UTC()

	rec.ExitCode, err = exitCodeFromWait(waitErr, cancellationErr)
	if err != nil {
		return rec, err
	}
	if err := stdout.Sync(); err != nil {
		return rec, err
	}
	if err := stderr.Sync(); err != nil {
		return rec, err
	}
	if info, statErr := stdout.Stat(); statErr == nil {
		rec.StdoutBytes = info.Size()
	} else {
		return rec, statErr
	}
	if info, statErr := stderr.Stat(); statErr == nil {
		rec.StderrBytes = info.Size()
	} else {
		return rec, statErr
	}
	meta, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return rec, err
	}
	if err := os.WriteFile(filepath.Join(dir, "record.json.tmp"), meta, 0600); err != nil {
		return rec, err
	}
	if err := os.Rename(filepath.Join(dir, "record.json.tmp"), filepath.Join(dir, "record.json")); err != nil {
		return rec, err
	}
	complete = true
	return rec, nil
}

// exitCodeFromWait preserves a finished child's status even if context cancellation
// raced with Wait. A timeout/interruption is reported only for signal exits.
func exitCodeFromWait(waitErr, cancellationErr error) (int, error) {
	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return 0, waitErr
	}
	if exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode(), nil
	}
	switch {
	case errors.Is(cancellationErr, context.DeadlineExceeded):
		return 124, nil
	case errors.Is(cancellationErr, context.Canceled):
		return 130, nil
	default:
		return 128 + signalNumber(exitErr), nil
	}
}

func recall(root, id, stream string, out io.Writer) error {
	if !validID.MatchString(id) {
		return errors.New("invalid run ID")
	}
	if stream != "stdout" && stream != "stderr" && stream != "both" {
		return errors.New("stream must be stdout, stderr, or both")
	}
	dir := filepath.Join(root, id)
	if _, err := os.Stat(filepath.Join(dir, "record.json")); err != nil {
		return fmt.Errorf("run %s: %w", id, err)
	}
	streams := []string{stream}
	if stream == "both" {
		streams = []string{"stdout", "stderr"}
	}
	for _, name := range streams {
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if stream == "both" {
			if _, err = fmt.Fprintf(out, "== %s ==\n", name); err != nil {
				file.Close()
				return err
			}
		}
		_, err = io.Copy(out, file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if stream == "both" {
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		}
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage:\n  ctxgo run [--timeout DURATION] [--summary-lines N] -- COMMAND [ARGS...]\n  ctxgo summary [--lines N] RUN_ID\n  ctxgo recall [--stream stdout|stderr|both] RUN_ID\n\nStored runs use CTXGO_DATA_DIR or the OS user cache directory.")
}

func runCLI(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		usage(errOut)
		return 2
	}
	root, err := dataDir()
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	switch args[0] {
	case "run":
		flags := flag.NewFlagSet("run", flag.ContinueOnError)
		flags.SetOutput(errOut)
		timeout := flags.Duration("timeout", 0, "maximum command execution time (0 means no timeout)")
		lines := flags.Int("summary-lines", defaultSummaryLines, "maximum number of filtered output lines (0 disables summary)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if *timeout < 0 || *lines < 0 || *lines > maxSummaryLines || len(flags.Args()) == 0 {
			fmt.Fprintln(errOut, "run requires a command, nonnegative timeout, and summary-lines between 0 and 100")
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if *timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, *timeout)
			defer cancel()
		}
		rec, err := execute(ctx, root, flags.Args())
		if err != nil {
			fmt.Fprintln(errOut, err)
			var pathErr *exec.Error
			if errors.As(err, &pathErr) && errors.Is(pathErr.Err, exec.ErrNotFound) {
				return 127
			}
			return 1
		}
		fmt.Fprintf(out, "run_id: %s\nexit_code: %d\nstdout_bytes: %d\nstderr_bytes: %d\n", rec.ID, rec.ExitCode, rec.StdoutBytes, rec.StderrBytes)
		fmt.Fprintf(out, "recall: ctxgo recall --stream stderr %s\n", rec.ID)
		if *lines > 0 {
			fmt.Fprintln(out, "summary (candidate diagnostics and recent lines):")
			if summaryErr := summarizeRun(root, rec.ID, *lines, out); summaryErr != nil {
				// Storage already succeeded; failure to display a preview must
				// not overwrite the child process exit status.
				fmt.Fprintf(errOut, "summary unavailable: %v\n", summaryErr)
			}
		}
		return rec.ExitCode
	case "summary":
		flags := flag.NewFlagSet("summary", flag.ContinueOnError)
		flags.SetOutput(errOut)
		lines := flags.Int("lines", defaultSummaryLines, "maximum number of filtered lines")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if len(flags.Args()) != 1 || *lines < 0 || *lines > maxSummaryLines {
			fmt.Fprintln(errOut, "summary requires one run ID and lines between 0 and 100")
			return 2
		}
		if err := summarizeRun(root, flags.Arg(0), *lines, out); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		return 0
	case "recall":
		flags := flag.NewFlagSet("recall", flag.ContinueOnError)
		flags.SetOutput(errOut)
		stream := flags.String("stream", "both", "stdout, stderr, or both")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if len(flags.Args()) != 1 {
			fmt.Fprintln(errOut, "recall requires exactly one run ID")
			return 2
		}
		if err := recall(root, flags.Arg(0), *stream, out); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		return 0
	case "help", "-h", "--help":
		usage(out)
		return 0
	default:
		fmt.Fprintf(errOut, "unknown command: %s\n", strings.TrimSpace(args[0]))
		usage(errOut)
		return 2
	}
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}
