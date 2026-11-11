// Package facts holds the typed activity facts persisted by the collector and the pure
// rules that derive them from Azure DevOps and Git data. It has no I/O dependencies.
package facts

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Identity kinds mirror the identities.kind database constraint.
const (
	KindADO        = "ado"
	KindGit        = "git"
	KindUnassigned = "unassigned"
	KindUnknown    = "unknown"
	KindFixture    = "fixture"
)

// Statistic states mirror the stats_status database constraints.
const (
	StatsOK            = "ok"
	StatsPartialBinary = "partial_binary"
	StatsUnavailable   = "unavailable"
	StatsMerge         = "merge"
	StatsPending       = "pending"
	StatsNotCompleted  = "not_completed"
)

type Identity struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName,omitempty"`
	IsService   bool   `json:"isService"`
}

var (
	Unassigned = Identity{ID: "unassigned", Kind: KindUnassigned, DisplayName: "Unassigned"}
	Unknown    = Identity{ID: "unknown", Kind: KindUnknown, DisplayName: "Unknown historical assignee"}
)

// AliasEmail is the Git email this identity can be matched by, or "" when its unique
// name is not an email address.
func (i Identity) AliasEmail() string {
	if i.Kind != KindADO || i.IsService || !strings.Contains(i.UniqueName, "@") {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(i.UniqueName))
}

// IdentityFromRef converts an Azure DevOps IdentityRef, as found in work item fields and
// pull request creators, to an Identity. Missing values are Unassigned; malformed ones are
// Unknown, never the current assignee.
func IdentityFromRef(v any) Identity {
	if v == nil || v == "" {
		return Unassigned
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Unknown
	}
	id, _ := m["id"].(string)
	if id == "" {
		return Unknown
	}
	name, _ := m["displayName"].(string)
	unique, _ := m["uniqueName"].(string)
	return NewADOIdentity(id, name, unique)
}

func NewADOIdentity(id, displayName, uniqueName string) Identity {
	if displayName == "" {
		displayName = uniqueName
	}
	if displayName == "" {
		displayName = id
	}
	return Identity{ID: strings.ToLower(id), Kind: KindADO, DisplayName: displayName, UniqueName: uniqueName, IsService: isService(displayName, uniqueName)}
}

// Build/service accounts are kept but flagged so reports can exclude them.
func isService(displayName, uniqueName string) bool {
	d := strings.ToLower(displayName)
	return strings.Contains(d, "build service") || strings.HasPrefix(d, "project collection") || strings.HasPrefix(d, "microsoft.") ||
		(uniqueName != "" && !strings.Contains(uniqueName, "@") && strings.Contains(uniqueName, "\\"))
}

type Repository struct {
	ID           string
	Organization string
	Project      string
	Name         string
	WebURL       string
}

type FileChange struct {
	Path      string
	OldPath   string
	Additions *int // nil for binary files
	Deletions *int
}

type Commit struct {
	SHA            string
	AuthorEmail    string
	AuthorName     string
	CommitterEmail string
	CommitterName  string
	AuthoredAt     time.Time
	CommittedAt    time.Time
	ParentCount    int
	Message        string
	Additions      *int
	Deletions      *int
	FilesChanged   *int
	BinaryFiles    *int
	StatsStatus    string
	Files          []FileChange
}

func (c Commit) IsMerge() bool { return c.ParentCount > 1 }

// DiffStats summarizes file changes. Text line counts are summed over text files only;
// binary files count toward Files and Binary and mark the result partial.
type DiffStats struct {
	Additions, Deletions, Files, Binary int
}

func Summarize(files []FileChange) DiffStats {
	s := DiffStats{Files: len(files)}
	for _, f := range files {
		if f.Additions == nil || f.Deletions == nil {
			s.Binary++
			continue
		}
		s.Additions += *f.Additions
		s.Deletions += *f.Deletions
	}
	return s
}

func (s DiffStats) Status() string {
	if s.Binary > 0 {
		return StatsPartialBinary
	}
	return StatsOK
}

// ApplyStats fills the commit's statistic columns from its file list. Merge commits carry
// no diff by convention and are excluded from churn totals.
func (c *Commit) ApplyStats() {
	if c.IsMerge() {
		c.Files = nil
		c.Additions, c.Deletions, c.FilesChanged, c.BinaryFiles = nil, nil, nil, nil
		c.StatsStatus = StatsMerge
		return
	}
	s := Summarize(c.Files)
	c.Additions, c.Deletions, c.FilesChanged, c.BinaryFiles = Int(s.Additions), Int(s.Deletions), Int(s.Files), Int(s.Binary)
	c.StatsStatus = s.Status()
}

type PullRequest struct {
	RepositoryID       string
	ID                 int
	Creator            Identity
	Title              string
	Status             string
	IsDraft            bool
	SourceRef          string
	TargetRef          string
	CreatedAt          time.Time
	ClosedAt           *time.Time
	CompletedAt        *time.Time
	LastMergeSourceSHA string
	LastMergeTargetSHA string
}

type PRStats struct {
	MergeBaseSHA string
	Diff         DiffStats
	Commits      int
}

type WorkItem struct {
	Organization  string
	Project       string
	ID            int
	Type          string
	Title         string
	CurrentState  string
	AreaPath      string
	IterationPath string
	CreatedAt     time.Time
	ChangedAt     time.Time
	LastRevision  int
}

type Revision struct {
	Rev    int
	Fields map[string]any
}

type TaskEvent struct {
	Revision   int
	Type       string // created | completed
	OccurredAt time.Time
	Identity   Identity
	FromState  string
	ToState    string
}

// CompletionStates is a case-insensitive set of work item states that mean "done".
type CompletionStates map[string]bool

func ParseCompletionStates(csv string) CompletionStates {
	s := CompletionStates{}
	for _, v := range strings.Split(csv, ",") {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			s[v] = true
		}
	}
	return s
}

func (s CompletionStates) Has(state string) bool { return s[strings.ToLower(strings.TrimSpace(state))] }

func fieldString(f map[string]any, key string) string { s, _ := f[key].(string); return s }

func parseTime(f map[string]any, key string) (time.Time, error) {
	v := fieldString(f, key)
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("field %s: invalid timestamp", key)
	}
	return t.UTC(), nil
}

// DeriveWorkItem rebuilds a work item's summary and its created/completed events from its
// complete revision history.
//
// Rules: creation happens at System.CreatedDate and belongs to the assignee in revision 1.
// A completion is an entry into the completion set from outside it, at that revision's
// System.ChangedDate, attributed to the assignee in that same revision. Moves within the set
// do not count again; reopening and re-entering counts again.
func DeriveWorkItem(org, project string, id int, revisions []Revision, done CompletionStates) (WorkItem, []TaskEvent, error) {
	if len(revisions) == 0 {
		return WorkItem{}, nil, fmt.Errorf("work item %d: no revisions", id)
	}
	revs := append([]Revision(nil), revisions...)
	sort.Slice(revs, func(i, j int) bool { return revs[i].Rev < revs[j].Rev })
	events := []TaskEvent{}
	previous := ""
	for i, r := range revs {
		if r.Rev != i+1 {
			return WorkItem{}, nil, fmt.Errorf("work item %d: incomplete revision history at revision %d", id, r.Rev)
		}
		state := strings.TrimSpace(fieldString(r.Fields, "System.State"))
		assignee := IdentityFromRef(r.Fields["System.AssignedTo"])
		if r.Rev == 1 {
			at, err := parseTime(r.Fields, "System.CreatedDate")
			if err != nil {
				return WorkItem{}, nil, fmt.Errorf("work item %d revision 1: %w", id, err)
			}
			events = append(events, TaskEvent{Revision: 1, Type: "created", OccurredAt: at, Identity: assignee, ToState: state})
		}
		if done.Has(state) && (r.Rev == 1 || !done.Has(previous)) {
			at, err := parseTime(r.Fields, "System.ChangedDate")
			if err != nil {
				return WorkItem{}, nil, fmt.Errorf("work item %d revision %d: %w", id, r.Rev, err)
			}
			events = append(events, TaskEvent{Revision: r.Rev, Type: "completed", OccurredAt: at, Identity: assignee, FromState: previous, ToState: state})
		}
		previous = state
	}
	first, last := revs[0].Fields, revs[len(revs)-1].Fields
	created, _ := parseTime(first, "System.CreatedDate")
	changed, err := parseTime(last, "System.ChangedDate")
	if err != nil {
		changed = created
	}
	w := WorkItem{
		Organization: org, Project: project, ID: id,
		Type:          fieldString(last, "System.WorkItemType"),
		Title:         fieldString(last, "System.Title"),
		CurrentState:  fieldString(last, "System.State"),
		AreaPath:      fieldString(last, "System.AreaPath"),
		IterationPath: fieldString(last, "System.IterationPath"),
		CreatedAt:     created, ChangedAt: changed, LastRevision: len(revs),
	}
	return w, events, nil
}

func Int(n int) *int { return &n }
