package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/ado"
)

func TestParseTime(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2026-10-15":                time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
		"2026-10-15T13:30":          time.Date(2026, 10, 15, 13, 30, 0, 0, time.UTC),
		"2026-10-15T06:00:00-07:00": time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC),
	} {
		got, err := parseTime(in)
		if err != nil || !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("parseTime(%q) = %v %v", in, got, err)
		}
	}
	if got, err := parseTime(""); err != nil || !got.IsZero() {
		t.Error("empty time")
	}
	if _, err := parseTime("10/15/2026"); err == nil {
		t.Error("bad format accepted")
	}
}

func TestList(t *testing.T) {
	got := list(" Widgets, ,Gadgets ,")
	if len(got) != 2 || got[0] != "Widgets" || got[1] != "Gadgets" {
		t.Fatalf("%q", got)
	}
	if len(list("")) != 0 {
		t.Fatal("empty list")
	}
}

func clearCreds(t *testing.T) {
	for _, k := range []string{"AZURE_DEVOPS_PAT", "AZURE_DEVOPS_BEARER_TOKEN", "AZURE_DEVOPS_TOKEN_COMMAND"} {
		t.Setenv(k, "")
	}
}

func TestTokensSelection(t *testing.T) {
	clearCreds(t)
	if _, err := tokens(); err == nil {
		t.Fatal("missing credentials accepted")
	}
	t.Setenv("AZURE_DEVOPS_PAT", " pat ")
	if tok, err := tokens(); err != nil || tok != ado.PAT("pat") {
		t.Fatalf("pat %v %v", tok, err)
	}
	t.Setenv("AZURE_DEVOPS_BEARER_TOKEN", "b")
	if _, err := tokens(); err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("mutual exclusion: %v", err)
	}
	t.Setenv("AZURE_DEVOPS_PAT", "")
	if tok, err := tokens(); err != nil || tok != ado.Bearer("b") {
		t.Fatalf("bearer %v %v", tok, err)
	}
	t.Setenv("AZURE_DEVOPS_BEARER_TOKEN", "")
	t.Setenv("AZURE_DEVOPS_TOKEN_COMMAND", "az account get-access-token --query accessToken")
	tok, err := tokens()
	ct, ok := tok.(*ado.CommandToken)
	if err != nil || !ok || len(ct.Command) != 5 || ct.Command[0] != "az" {
		t.Fatalf("command %#v %v", tok, err)
	}
}

func TestRunValidationBeforeConnecting(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	base := func(t *testing.T) {
		clearCreds(t)
		for _, k := range []string{"SYNC_OVERLAP", "SYNC_BOOTSTRAP", "RECONCILE_EVERY", "RECONCILE_WINDOW", "COLLECTOR_CONCURRENCY", "COMPLETION_STATES", "WORK_ITEM_TYPES", "AZURE_DEVOPS_REPOSITORIES", "AZURE_DEVOPS_REPOSITORY"} {
			t.Setenv(k, "")
		}
		// Unreachable database: any test reaching the connection step would fail differently.
		t.Setenv("DATABASE_URL", "postgres://invalid:invalid@127.0.0.1:1/invalid?connect_timeout=1")
		t.Setenv("AZURE_DEVOPS_PAT", "x")
		t.Setenv("AZURE_DEVOPS_ORG", "acme")
		t.Setenv("AZURE_DEVOPS_PROJECT", "Platform")
		t.Setenv("AZURE_DEVOPS_REPOSITORIES", "Widgets")
	}
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"unknown source", []string{"--sources", "commits,builds"}, nil, "unknown source"},
		{"from with months", []string{"--from", "2026-01-01", "--months", "3"}, nil, "either --from"},
		{"bad from", []string{"--from", "yesterday"}, nil, "invalid time"},
		{"bad overlap", nil, map[string]string{"SYNC_OVERLAP": "two hours"}, "SYNC_OVERLAP"},
		{"bad concurrency", nil, map[string]string{"COLLECTOR_CONCURRENCY": "99"}, "COLLECTOR_CONCURRENCY"},
		{"missing database", nil, map[string]string{"DATABASE_URL": ""}, "DATABASE_URL is required"},
		{"missing credentials", nil, map[string]string{"AZURE_DEVOPS_PAT": ""}, "credentials required"},
		{"missing organization", nil, map[string]string{"AZURE_DEVOPS_ORG": ""}, "AZURE_DEVOPS_ORG and AZURE_DEVOPS_PROJECT are required"},
		{"missing repositories", nil, map[string]string{"AZURE_DEVOPS_REPOSITORIES": ""}, "repositories, work item types"},
		{"unexpected argument", []string{"go"}, nil, "unexpected argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			err := run(tc.args, log)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
