// Package gitmirror maintains a bare mirror of an Azure Repos Git repository and extracts
// commit facts and diff statistics from it. Azure DevOps APIs report file change counts but
// not line additions/deletions, so line statistics come from Git.
package gitmirror

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/facts"
)

// Mirror is a bare repository fetched from URL. AuthHeader, when set, returns the HTTP
// Authorization header value for each Git invocation (tokens may be refreshed).
type Mirror struct {
	Dir        string
	URL        string
	AuthHeader func(context.Context) (string, error)
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func ValidSHA(s string) bool { return shaPattern.MatchString(s) }

// ErrGit is returned for failed Git commands. Output is withheld because it can echo URLs
// or headers; stderr is reduced to a bounded first line with credentials stripped.
type ErrGit struct {
	Command string
	Detail  string
}

func (e *ErrGit) Error() string {
	if e.Detail == "" {
		return "git " + e.Command + " failed"
	}
	return "git " + e.Command + " failed: " + e.Detail
}

func (m *Mirror) command(ctx context.Context, stdin io.Reader, args ...string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", m.Dir}, args...)...)
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "LC_ALL=C")
	if m.AuthHeader != nil && (args[0] == "fetch" || args[0] == "ls-remote") {
		h, err := m.AuthHeader(ctx)
		if err != nil {
			return nil, err
		}
		// Passing the header through config environment keeps it out of argv and .git/config.
		env = append(env, "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: "+h,
			"GIT_CONFIG_KEY_1=credential.helper", "GIT_CONFIG_VALUE_1=")
	}
	cmd.Env = env
	cmd.Stdin = stdin
	return cmd, nil
}

var secretish = regexp.MustCompile(`(?i)(authorization|bearer|basic|https?://[^\s]*@)[^\s]*`)

func sanitize(stderr []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(stderr)), "\n")
	line = secretish.ReplaceAllString(line, "[redacted]")
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

func (m *Mirror) run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd, err := m.command(ctx, stdin, args...)
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &ErrGit{Command: args[0], Detail: sanitize(stderr.Bytes())}
	}
	return stdout.String(), nil
}

// Fetch creates the bare mirror if needed and updates every branch. Branches deleted
// upstream are intentionally not pruned, so previously observed history stays available.
func (m *Mirror) Fetch(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(m.Dir), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if _, err := m.run(ctx, nil, "init", "--bare", "--quiet", m.Dir); err != nil {
			return err
		}
		// Merge-base and rename detection are cheaper without automatic GC during runs.
		if _, err := m.run(ctx, nil, "config", "gc.auto", "0"); err != nil {
			return err
		}
	}
	_, err := m.run(ctx, nil, "fetch", "--quiet", "--no-tags", "--force", m.URL, "+refs/heads/*:refs/heads/*")
	return err
}

// FetchCommit retrieves a specific commit (for example a completed PR's source snapshot on
// a deleted branch) when it is missing locally.
func (m *Mirror) FetchCommit(ctx context.Context, sha string) error {
	if !ValidSHA(sha) {
		return fmt.Errorf("invalid commit id")
	}
	if m.HasCommit(ctx, sha) {
		return nil
	}
	_, err := m.run(ctx, nil, "fetch", "--quiet", "--no-tags", m.URL, sha)
	if err == nil && !m.HasCommit(ctx, sha) {
		return fmt.Errorf("commit %s unavailable after fetch", sha[:12])
	}
	return err
}

func (m *Mirror) HasCommit(ctx context.Context, sha string) bool {
	_, err := m.run(ctx, nil, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// RefTips returns every local branch and its commit.
func (m *Mirror) RefTips(ctx context.Context) (map[string]string, error) {
	out, err := m.run(ctx, nil, "for-each-ref", "--format=%(refname)%00%(objectname)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	tips := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		ref, sha, ok := strings.Cut(line, "\x00")
		if !ok || !ValidSHA(sha) {
			return nil, fmt.Errorf("unexpected for-each-ref output")
		}
		tips[ref] = sha
	}
	return tips, nil
}

// LogOptions selects commits reachable from all branches. Since filters by committer
// time; Exclude lists commits whose ancestry was already collected (previous branch tips).
type LogOptions struct {
	Since   time.Time
	Until   time.Time
	Exclude []string
}

const (
	recordSep = "\x1e"
	fieldSep  = "\x1f"
	// %B is last because it is free text; numstat output follows the final separator.
	logFormat = "--format=" + recordSep + "%H" + fieldSep + "%P" + fieldSep + "%ae" + fieldSep + "%an" + fieldSep + "%at" + fieldSep + "%ce" + fieldSep + "%cn" + fieldSep + "%ct" + fieldSep + "%B" + fieldSep
)

// Log streams commits with per-file statistics to fn. Each unique commit is reported once
// regardless of how many branches contain it. Merge commits are reported without files.
func (m *Mirror) Log(ctx context.Context, opts LogOptions, fn func(facts.Commit) error) error {
	args := []string{"log", "--all", "--no-color", "--numstat", "-z", "-M", "--no-ext-diff", "--no-textconv", logFormat}
	if !opts.Since.IsZero() {
		args = append(args, "--since-as-filter="+strconv.FormatInt(opts.Since.Unix(), 10))
	}
	if !opts.Until.IsZero() {
		args = append(args, "--until="+strconv.FormatInt(opts.Until.Unix(), 10))
	}
	var stdin io.Reader
	if len(opts.Exclude) > 0 {
		args = append(args, "--ignore-missing", "--stdin")
		var b strings.Builder
		for _, sha := range opts.Exclude {
			if ValidSHA(sha) {
				b.WriteString("^" + sha + "\n")
			}
		}
		stdin = strings.NewReader(b.String())
	}
	cmd, err := m.command(ctx, stdin, args...)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return &ErrGit{Command: "log", Detail: err.Error()}
	}
	parseErr := parseLog(out, fn)
	if parseErr != nil {
		_ = cmd.Process.Kill()
		_, _ = io.Copy(io.Discard, out)
	}
	waitErr := cmd.Wait()
	if parseErr != nil {
		return parseErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &ErrGit{Command: "log", Detail: sanitize(stderr.Bytes())}
	}
	return nil
}

func parseLog(r io.Reader, fn func(facts.Commit) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 256<<20)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		start := 0
		if len(data) > 0 && data[0] == recordSep[0] {
			start = 1
		}
		if i := bytes.IndexByte(data[start:], recordSep[0]); i >= 0 {
			return start + i, data[start : start+i], nil
		}
		if atEOF && len(data) > start {
			return len(data), data[start:], nil
		}
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		record := scanner.Text()
		if strings.Trim(record, "\x00\n") == "" {
			continue
		}
		c, err := parseRecord(record)
		if err != nil {
			return err
		}
		if err := fn(c); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func parseRecord(record string) (facts.Commit, error) {
	parts := strings.SplitN(record, fieldSep, 10)
	if len(parts) != 10 {
		return facts.Commit{}, fmt.Errorf("invalid git log record")
	}
	sha := parts[0]
	if !ValidSHA(sha) {
		return facts.Commit{}, fmt.Errorf("invalid commit id in git log")
	}
	at, err1 := strconv.ParseInt(parts[4], 10, 64)
	ct, err2 := strconv.ParseInt(parts[7], 10, 64)
	if err1 != nil || err2 != nil {
		return facts.Commit{}, fmt.Errorf("commit %s: invalid timestamp", sha[:12])
	}
	c := facts.Commit{
		SHA:            sha,
		ParentCount:    len(strings.Fields(parts[1])),
		AuthorEmail:    strings.ToLower(strings.TrimSpace(parts[2])),
		AuthorName:     strings.TrimSpace(parts[3]),
		AuthoredAt:     time.Unix(at, 0).UTC(),
		CommitterEmail: strings.ToLower(strings.TrimSpace(parts[5])),
		CommitterName:  strings.TrimSpace(parts[6]),
		CommittedAt:    time.Unix(ct, 0).UTC(),
		Message:        strings.TrimRight(strings.ToValidUTF8(strings.ReplaceAll(parts[8], "\x00", ""), "�"), "\n"),
	}
	if c.AuthorName == "" {
		c.AuthorName = c.AuthorEmail
	}
	files, err := ParseNumstat(parts[9])
	if err != nil {
		return facts.Commit{}, fmt.Errorf("commit %s: %w", sha[:12], err)
	}
	c.Files = files
	c.ApplyStats()
	return c, nil
}

// ParseNumstat parses `--numstat -z` output: "added\tdeleted\tpath\0" for ordinary entries
// and "added\tdeleted\t\0old\0new\0" for renames/copies. Binary files report "-".
func ParseNumstat(raw string) ([]facts.FileChange, error) {
	tokens := strings.Split(strings.TrimLeft(raw, "\n\x00"), "\x00")
	files := []facts.FileChange{}
	for i := 0; i < len(tokens); i++ {
		token := strings.TrimLeft(tokens[i], "\n")
		if token == "" {
			continue
		}
		cols := strings.SplitN(token, "\t", 3)
		if len(cols) != 3 {
			return nil, fmt.Errorf("invalid numstat entry")
		}
		f := facts.FileChange{Path: cols[2]}
		if cols[0] != "-" || cols[1] != "-" {
			a, err := strconv.Atoi(cols[0])
			if err != nil {
				return nil, fmt.Errorf("invalid numstat additions")
			}
			d, err := strconv.Atoi(cols[1])
			if err != nil {
				return nil, fmt.Errorf("invalid numstat deletions")
			}
			f.Additions, f.Deletions = facts.Int(a), facts.Int(d)
		}
		if cols[2] == "" {
			if i+2 >= len(tokens) || tokens[i+2] == "" {
				return nil, fmt.Errorf("incomplete rename in numstat")
			}
			f.OldPath, f.Path = tokens[i+1], tokens[i+2]
			i += 2
		}
		files = append(files, f)
	}
	return files, nil
}

// PRStats computes the completed PR snapshot: the diff from merge-base(target, source) to
// source, and the number of commits in base..source. Missing commits are fetched by id.
func (m *Mirror) PRStats(ctx context.Context, sourceSHA, targetSHA string) (facts.PRStats, error) {
	if !ValidSHA(sourceSHA) || !ValidSHA(targetSHA) {
		return facts.PRStats{}, fmt.Errorf("PR snapshot commits unavailable")
	}
	for _, sha := range []string{sourceSHA, targetSHA} {
		if err := m.FetchCommit(ctx, sha); err != nil {
			return facts.PRStats{}, err
		}
	}
	base, err := m.run(ctx, nil, "merge-base", targetSHA, sourceSHA)
	if err != nil {
		return facts.PRStats{}, err
	}
	base = strings.TrimSpace(base)
	if !ValidSHA(base) {
		return facts.PRStats{}, fmt.Errorf("no merge base")
	}
	diff, err := m.run(ctx, nil, "diff", "--numstat", "-z", "-M", "--no-ext-diff", "--no-textconv", base, sourceSHA, "--")
	if err != nil {
		return facts.PRStats{}, err
	}
	files, err := ParseNumstat(diff)
	if err != nil {
		return facts.PRStats{}, err
	}
	count, err := m.run(ctx, nil, "rev-list", "--count", base+".."+sourceSHA)
	if err != nil {
		return facts.PRStats{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil {
		return facts.PRStats{}, fmt.Errorf("invalid rev-list count")
	}
	return facts.PRStats{MergeBaseSHA: base, Diff: facts.Summarize(files), Commits: n}, nil
}
