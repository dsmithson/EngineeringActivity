// Command collector is the run-to-completion Azure DevOps activity collector. It is run
// hourly by a scheduler (Kubernetes CronJob) or manually for backfills, and exits nonzero
// when any source fails.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/ado"
	"github.com/dsmithson/EngineeringActivity/internal/collector"
	"github.com/dsmithson/EngineeringActivity/internal/facts"
	"github.com/dsmithson/EngineeringActivity/internal/gitmirror"
	"github.com/dsmithson/EngineeringActivity/internal/store"
)

const defaultCompletionStates = "ready for qa test build,ready to test,closed"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).With("service", "collector")
	if err := run(os.Args[1:], log); err != nil {
		log.Error("collector exited with error", "error_class", collector.Classify(err), "detail", safeDetail(err))
		os.Exit(1)
	}
}

// safeDetail keeps configuration/validation messages (which never contain secrets) and
// suppresses upstream messages, which are bounded by class instead.
func safeDetail(err error) string {
	switch collector.Classify(err) {
	case "config", "other":
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return msg
	}
	return ""
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := env(key, "")
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a Go duration such as 2h", key)
	}
	return d, nil
}

func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q (use RFC3339 or YYYY-MM-DD, interpreted as UTC)", v)
}

func list(csv string) []string {
	out := []string{}
	for _, v := range strings.Split(csv, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func tokens() (ado.TokenSource, error) {
	pat, bearer, command := os.Getenv("AZURE_DEVOPS_PAT"), os.Getenv("AZURE_DEVOPS_BEARER_TOKEN"), os.Getenv("AZURE_DEVOPS_TOKEN_COMMAND")
	set := 0
	for _, v := range []string{pat, bearer, command} {
		if v != "" {
			set++
		}
	}
	switch {
	case set > 1:
		return nil, errors.New("set only one of AZURE_DEVOPS_PAT, AZURE_DEVOPS_BEARER_TOKEN, AZURE_DEVOPS_TOKEN_COMMAND")
	case pat != "":
		return ado.PAT(strings.TrimSpace(pat)), nil
	case bearer != "":
		return ado.Bearer(strings.TrimSpace(bearer)), nil
	case command != "":
		return &ado.CommandToken{Command: strings.Fields(command), TTL: 30 * time.Minute}, nil
	}
	return nil, errors.New("Azure DevOps credentials required: set AZURE_DEVOPS_PAT (cluster), or AZURE_DEVOPS_TOKEN_COMMAND / AZURE_DEVOPS_BEARER_TOKEN (local Entra token)")
}

func run(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("collector", flag.ContinueOnError)
	mode := fs.String("mode", collector.ModeScheduled, "scheduled | reconcile | backfill | window")
	from := fs.String("from", "", "start (UTC, RFC3339 or YYYY-MM-DD) for backfill/window")
	to := fs.String("to", "", "end (exclusive) for window mode")
	months := fs.Int("months", 0, "backfill shortcut: start this many months before now")
	days := fs.Int("days", 0, "backfill shortcut: start this many days before now")
	sources := fs.String("sources", "", "comma-separated subset of commits,pull_requests,tasks (default all)")
	allowFixture := fs.Bool("allow-fixture-db", false, "permit writing live data into a database containing development fixtures")
	deadline := fs.Duration("deadline", 0, "maximum run time (default 50m scheduled, 12h backfill)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 && fs.Arg(0) != "run" {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	req := collector.Request{Mode: *mode, Sources: map[string]bool{}}
	var err error
	if req.From, err = parseTime(*from); err != nil {
		return err
	}
	if req.To, err = parseTime(*to); err != nil {
		return err
	}
	if *months > 0 || *days > 0 {
		if *from != "" {
			return errors.New("use either --from or --months/--days")
		}
		req.From = time.Now().UTC().AddDate(0, -*months, -*days).Truncate(24 * time.Hour)
		if req.Mode == collector.ModeScheduled {
			req.Mode = collector.ModeBackfill
		}
	}
	for _, s := range list(*sources) {
		if s != collector.SourceCommits && s != collector.SourcePullRequests && s != collector.SourceTasks {
			return fmt.Errorf("unknown source %q", s)
		}
		req.Sources[s] = true
	}

	org := os.Getenv("AZURE_DEVOPS_ORG")
	cfg := collector.Config{
		Organization:     org,
		Project:          os.Getenv("AZURE_DEVOPS_PROJECT"),
		Repositories:     list(env("AZURE_DEVOPS_REPOSITORIES", os.Getenv("AZURE_DEVOPS_REPOSITORY"))),
		WorkItemTypes:    list(env("WORK_ITEM_TYPES", "Task")),
		CompletionStates: facts.ParseCompletionStates(env("COMPLETION_STATES", defaultCompletionStates)),
		AllowFixtureDB:   *allowFixture,
	}
	if cfg.Overlap, err = envDuration("SYNC_OVERLAP", 2*time.Hour); err != nil {
		return err
	}
	if cfg.Bootstrap, err = envDuration("SYNC_BOOTSTRAP", 7*24*time.Hour); err != nil {
		return err
	}
	if cfg.ReconcileEvery, err = envDuration("RECONCILE_EVERY", 24*time.Hour); err != nil {
		return err
	}
	if cfg.ReconcileWindow, err = envDuration("RECONCILE_WINDOW", 7*24*time.Hour); err != nil {
		return err
	}
	if cfg.Concurrency, err = strconv.Atoi(env("COLLECTOR_CONCURRENCY", "4")); err != nil || cfg.Concurrency < 1 || cfg.Concurrency > 16 {
		return errors.New("COLLECTOR_CONCURRENCY must be 1-16")
	}
	if cfg.Organization == "" || cfg.Project == "" {
		return errors.New("AZURE_DEVOPS_ORG and AZURE_DEVOPS_PROJECT are required")
	}
	if len(cfg.Repositories) == 0 || len(cfg.CompletionStates) == 0 || len(cfg.WorkItemTypes) == 0 {
		return errors.New("repositories, work item types, and completion states must not be empty")
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	tok, err := tokens()
	if err != nil {
		return err
	}

	if *deadline == 0 {
		*deadline = 50 * time.Minute
		if req.Mode == collector.ModeBackfill || req.Mode == collector.ModeWindow {
			*deadline = 12 * time.Hour
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *deadline)
	defer cancel()

	db, err := store.Open(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	orgURL := env("AZURE_DEVOPS_URL", "https://dev.azure.com/"+url.PathEscape(org))
	counters := &ado.Counters{}
	client := ado.New(orgURL, org, cfg.Project, ado.NewHTTPClient(tok, counters, 90*time.Second))
	mirrorDir := env("GIT_MIRROR_DIR", filepath.Join("data", "mirrors"))
	c := &collector.Collector{
		Config:   cfg,
		ADO:      client,
		Store:    db,
		Counters: counters,
		Log:      log,
		Mirrors: func(r facts.Repository) collector.Mirror {
			return &gitmirror.Mirror{
				Dir:        filepath.Join(mirrorDir, r.Name+".git"),
				URL:        strings.TrimRight(orgURL, "/") + "/" + url.PathEscape(r.Project) + "/_git/" + url.PathEscape(r.Name),
				AuthHeader: tok.Header,
			}
		},
	}
	return c.Run(ctx, req)
}
