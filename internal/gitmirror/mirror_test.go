package gitmirror

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/facts"
)

func TestParseNumstat(t *testing.T) {
	files, err := ParseNumstat("12\t3\tfile\x002\t1\t\x00old name\x00new\tname\x00-\t-\timage.png\x00")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[1].OldPath != "old name" || files[1].Path != "new\tname" || files[2].Additions != nil {
		t.Fatalf("unexpected parse: %+v", files)
	}
	s := facts.Summarize(files)
	if s.Additions != 14 || s.Deletions != 4 || s.Files != 3 || s.Binary != 1 || s.Status() != facts.StatsPartialBinary {
		t.Fatalf("summary %+v", s)
	}
	if _, err := ParseNumstat("bad record\x00"); err == nil {
		t.Fatal("malformed numstat accepted")
	}
	if _, err := ParseNumstat("1\t1\t\x00only-old\x00"); err == nil {
		t.Fatal("truncated rename accepted")
	}
}

type repo struct {
	t   *testing.T
	dir string
	n   int
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "--quiet", "--initial-branch=main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "core.autocrlf=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ada Author", "GIT_AUTHOR_EMAIL=Ada@Example.Test", "GIT_COMMITTER_NAME=Cy Committer", "GIT_COMMITTER_EMAIL=cy@example.test")
	b, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %s %v", args, b, err)
	}
	return strings.TrimSpace(string(b))
}

func (r *repo) write(name, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit(message string) string {
	r.t.Helper()
	r.n++
	r.git("add", "-A")
	r.git("commit", "--quiet", "--allow-empty", "-m", message)
	return r.git("rev-parse", "HEAD")
}

func collect(t *testing.T, m *Mirror, opts LogOptions) map[string]facts.Commit {
	t.Helper()
	out := map[string]facts.Commit{}
	if err := m.Log(context.Background(), opts, func(c facts.Commit) error {
		if _, dup := out[c.SHA]; dup {
			t.Fatalf("commit %s reported twice", c.SHA)
		}
		out[c.SHA] = c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMirrorLogStatsRenamesBinaryMergesAndBranches(t *testing.T) {
	r := newRepo(t)
	root := r.commit("root\n\nempty tree")
	r.write("src/top.vhd", "a\nb\nc\n")
	r.write("img.bin", "\x00\x01\x02binary")
	text := r.commit("add files\n\nBody line\n\nCo-Authored-By: Claude <noreply@anthropic.com>")
	r.git("checkout", "--quiet", "-b", "feature")
	r.git("mv", "src/top.vhd", "src/renamed top.vhd")
	r.write("src/renamed top.vhd", "a\nb\nc\nd\n")
	rename := r.commit("rename with spaces")
	r.git("checkout", "--quiet", "main")
	r.write("main.go", "package main\n")
	mainOnly := r.commit("main work")
	r.git("merge", "--quiet", "--no-ff", "feature", "-m", "merge feature")
	merge := r.git("rev-parse", "HEAD")
	r.git("branch", "also-feature", "feature")

	ctx := context.Background()
	m := &Mirror{Dir: filepath.Join(t.TempDir(), "mirror.git"), URL: r.dir}
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	all := collect(t, m, LogOptions{})
	if len(all) != 5 {
		t.Fatalf("expected 5 unique commits across 3 branches, got %d", len(all))
	}
	if c := all[root]; c.StatsStatus != facts.StatsOK || *c.FilesChanged != 0 || c.Message != "root\n\nempty tree" {
		t.Fatalf("root commit: %+v", c)
	}
	c := all[text]
	if c.AuthorEmail != "ada@example.test" || c.AuthorName != "Ada Author" || c.CommitterEmail != "cy@example.test" {
		t.Fatalf("identity fields: %+v", c)
	}
	if *c.FilesChanged != 2 || *c.BinaryFiles != 1 || *c.Additions != 3 || c.StatsStatus != facts.StatsPartialBinary {
		t.Fatalf("binary stats: %+v", c)
	}
	if !strings.Contains(c.Message, "Co-Authored-By: Claude") {
		t.Fatalf("message trailers lost: %q", c.Message)
	}
	rc := all[rename]
	if len(rc.Files) != 1 || rc.Files[0].OldPath != "src/top.vhd" || rc.Files[0].Path != "src/renamed top.vhd" || *rc.Additions != 1 {
		t.Fatalf("rename: %+v", rc.Files)
	}
	if mc := all[merge]; !mc.IsMerge() || mc.StatsStatus != facts.StatsMerge || mc.Files != nil {
		t.Fatalf("merge: %+v", mc)
	}

	// Incremental: excluding previous tips yields only commits not yet seen.
	tips, err := m.RefTips(ctx)
	if err != nil || len(tips) != 3 {
		t.Fatalf("tips %v %v", tips, err)
	}
	r.write("main.go", "package main\n// more\n")
	later := r.commit("later")
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	exclude := []string{"0000000000000000000000000000000000000000"}
	for _, sha := range tips {
		exclude = append(exclude, sha)
	}
	inc := collect(t, m, LogOptions{Exclude: exclude})
	if len(inc) != 1 || inc[later].SHA != later {
		t.Fatalf("incremental listed %d commits", len(inc))
	}
	_ = mainOnly

	// PR snapshot: feature branch against main before the merge.
	stats, err := m.PRStats(ctx, rename, mainOnly)
	if err != nil {
		t.Fatal(err)
	}
	if stats.MergeBaseSHA != text || stats.Commits != 1 || stats.Diff.Files != 1 || stats.Diff.Additions != 1 {
		t.Fatalf("PR stats %+v", stats)
	}
	if _, err := m.PRStats(ctx, "zz", mainOnly); err == nil {
		t.Fatal("invalid sha accepted")
	}
	since := collect(t, m, LogOptions{Since: time.Now().Add(time.Hour)})
	if len(since) != 0 {
		t.Fatalf("since filter returned %d", len(since))
	}
}
