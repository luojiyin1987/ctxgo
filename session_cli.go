package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

func sessionUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ctxgo session start TASK...")
	fmt.Fprintln(w, "  ctxgo session add [--kind decision|constraint|progress|result|next] SESSION_ID MESSAGE...")
	fmt.Fprintln(w, "  ctxgo session show [--limit N] [--before EVENT_ID] SESSION_ID")
	fmt.Fprintln(w, "  ctxgo session list [--limit N]")
	fmt.Fprintln(w, "  ctxgo session close SESSION_ID")
}

func runSessionCLI(root string, args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		sessionUsage(errOut)
		return 2
	}
	switch args[0] {
	case "start":
		if len(args) < 2 {
			fmt.Fprintln(errOut, "session start requires a task description")
			return 2
		}
		db, err := openSessionStore(root)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		defer db.Close()
		id, err := newSession(db, strings.Join(args[1:], " "))
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 2
		}
		fmt.Fprintf(out, "session_id: %s\n", id)
		return 0
	case "add":
		flags := flag.NewFlagSet("session add", flag.ContinueOnError)
		flags.SetOutput(errOut)
		kind := flags.String("kind", "progress", "event kind")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if len(flags.Args()) < 2 {
			fmt.Fprintln(errOut, "session add requires a session ID and event text")
			return 2
		}
		if !validID.MatchString(flags.Arg(0)) || !validSessionKind(*kind) ||
			!validSessionText(strings.Join(flags.Args()[1:], " "), maxSessionEventBytes) {
			fmt.Fprintln(errOut, "invalid session ID, kind, or event text")
			return 2
		}
		db, err := openSessionStore(root)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		defer db.Close()
		if err := addSessionEvent(db, flags.Arg(0), *kind, strings.Join(flags.Args()[1:], " ")); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		fmt.Fprintln(out, "event recorded")
		return 0
	case "show":
		flags := flag.NewFlagSet("session show", flag.ContinueOnError)
		flags.SetOutput(errOut)
		limit := flags.Int("limit", 30, "maximum number of most recent events (1-100)")
		before := flags.Int64("before", 0, "exclusive event ID cursor (0 means newest)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if len(flags.Args()) != 1 || *limit < 1 || *limit > maxSessionResults ||
			*before < 0 || !validID.MatchString(flags.Arg(0)) {
			fmt.Fprintln(errOut, "session show requires one session ID and limit between 1 and 100")
			return 2
		}
		db, err := openSessionStore(root)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		defer db.Close()
		snapshot, err := loadSessionBefore(db, flags.Arg(0), *limit, *before)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		fmt.Fprintf(out, "session_id: %s\nstatus: %s\ntask: %q\ncreated_at: %s\n",
			snapshot.Session.ID, snapshot.Session.Status, snapshot.Session.Task, snapshot.Session.CreatedAt)
		if snapshot.Session.ClosedAt.Valid {
			fmt.Fprintf(out, "closed_at: %s\n", snapshot.Session.ClosedAt.String)
		}
		fmt.Fprintf(out, "events: %d of %d\n", len(snapshot.Events), snapshot.TotalEvents)
		if snapshot.More && len(snapshot.Events) > 0 {
			fmt.Fprintf(out, "older events omitted; page with: ctxgo session show --limit %d --before %d %s\n",
				*limit, snapshot.Events[0].ID, snapshot.Session.ID)
		}
		for _, event := range snapshot.Events {
			fmt.Fprintf(out, "  #%d %s [%s] %q\n", event.ID, event.CreatedAt, event.Kind, event.Text)
		}
		return 0
	case "list":
		flags := flag.NewFlagSet("session list", flag.ContinueOnError)
		flags.SetOutput(errOut)
		limit := flags.Int("limit", 20, "maximum sessions (1-100)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if len(flags.Args()) != 0 || *limit < 1 || *limit > maxSessionResults {
			fmt.Fprintln(errOut, "session list accepts only --limit N (1-100)")
			return 2
		}
		db, err := openSessionStore(root)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		defer db.Close()
		sessions, err := listSessions(db, *limit)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		for _, item := range sessions {
			fmt.Fprintf(out, "%s %s %s %q\n", item.ID, item.Status, item.CreatedAt, item.Task)
		}
		return 0
	case "close":
		if len(args) != 2 || !validID.MatchString(args[1]) {
			fmt.Fprintln(errOut, "session close requires one valid session ID")
			return 2
		}
		db, err := openSessionStore(root)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		defer db.Close()
		if err := closeSession(db, args[1]); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		fmt.Fprintln(out, "session closed")
		return 0
	case "help", "-h", "--help":
		sessionUsage(out)
		return 0
	default:
		sessionUsage(errOut)
		return 2
	}
}
