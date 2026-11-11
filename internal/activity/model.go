// Package activity provides the in-memory sample data used by DEMO_MODE previews. Live
// and fixture data are aggregated in PostgreSQL (internal/store).
package activity

import (
	"fmt"
	"sort"
	"time"
)

type Identity struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}
type Event struct {
	Key     string    `json:"key"`
	Kind    string    `json:"kind"`
	At      time.Time `json:"at"`
	User    Identity  `json:"user"`
	Added   *int      `json:"added"`
	Deleted *int      `json:"deleted"`
	Files   *int      `json:"files"`
	Commits *int      `json:"commits"`
}
type Bucket struct {
	At            time.Time `json:"at"`
	PRCreated     int       `json:"prCreated"`
	PRCompleted   int       `json:"prCompleted"`
	PRAdded       int       `json:"prAdded"`
	PRDeleted     int       `json:"prDeleted"`
	PRFiles       int       `json:"prFiles"`
	PRCommits     int       `json:"prCommits"`
	PRUnknown     int       `json:"prUnknown"`
	Commits       int       `json:"commits"`
	Added         int       `json:"added"`
	Deleted       int       `json:"deleted"`
	Files         int       `json:"files"`
	CommitUnknown int       `json:"commitUnknown"`
	Merges        int       `json:"merges"`
	TaskCreated   int       `json:"taskCreated"`
	TaskCompleted int       `json:"taskCompleted"`
}
type Result struct {
	Buckets         []Bucket   `json:"buckets"`
	Identities      []Identity `json:"identities"`
	Start           time.Time  `json:"start"`
	End             time.Time  `json:"end"`
	PartialLastHour bool       `json:"partialLastHour"`
	Demo            bool       `json:"demo"`
	Sources         any        `json:"sources"`
}

func Int(n int) *int { return &n }
func add(p *int, n *int) {
	if n != nil {
		*p += *n
	}
}
func Window(now time.Time) (time.Time, time.Time) {
	return now.UTC().Truncate(time.Hour).Add(-167 * time.Hour), now.UTC()
}
func Aggregate(events []Event, now time.Time, user string) Result {
	start, end := Window(now)
	r := Result{Start: start, End: end, PartialLastHour: true, Buckets: make([]Bucket, 168), Identities: []Identity{}}
	for i := range r.Buckets {
		r.Buckets[i].At = start.Add(time.Duration(i) * time.Hour)
	}
	users := map[string]Identity{}
	seen := map[string]bool{}
	for _, e := range events {
		if seen[e.Key] || e.At.Before(start) || e.At.After(end) {
			continue
		}
		seen[e.Key] = true
		users[e.User.ID] = e.User
		if user != "" && e.User.ID != user {
			continue
		}
		i := int(e.At.Sub(start) / time.Hour)
		if i < 0 || i >= len(r.Buckets) {
			continue
		}
		b := &r.Buckets[i]
		switch e.Kind {
		case "pr_created":
			b.PRCreated++
		case "pr_completed":
			b.PRCompleted++
			add(&b.PRAdded, e.Added)
			add(&b.PRDeleted, e.Deleted)
			add(&b.PRFiles, e.Files)
			add(&b.PRCommits, e.Commits)
			if e.Added == nil || e.Deleted == nil || e.Files == nil || e.Commits == nil {
				b.PRUnknown++
			}
		case "commit":
			b.Commits++
			add(&b.Added, e.Added)
			add(&b.Deleted, e.Deleted)
			add(&b.Files, e.Files)
			if e.Added == nil || e.Deleted == nil || e.Files == nil {
				b.CommitUnknown++
			}
		case "merge":
			b.Merges++
		case "task_created":
			b.TaskCreated++
		case "task_completed":
			b.TaskCompleted++
		}
	}
	for _, u := range users {
		r.Identities = append(r.Identities, u)
	}
	sort.Slice(r.Identities, func(i, j int) bool { return r.Identities[i].Name < r.Identities[j].Name })
	return r
}

func Demo(now time.Time) []Event {
	start, _ := Window(now)
	users := []Identity{{ID: "sample-a", Name: "Sample developer A"}, {ID: "sample-b", Name: "Sample developer B"}}
	events := []Event{}
	for i := 0; i < 168; i++ {
		if i%7 == 0 {
			continue
		}
		u := users[i%2]
		at := start.Add(time.Duration(i) * time.Hour)
		for j := 0; j < (i*13)%6; j++ {
			events = append(events, Event{Key: fmt.Sprintf("demo:commit:%d:%d", i, j), Kind: "commit", At: at, User: u, Added: Int(15 + (i*31+j)%180), Deleted: Int((i*7 + j) % 60), Files: Int(1 + i%8)})
		}
		if i%3 == 0 {
			for _, kind := range []string{"pr_created", "pr_completed", "task_created", "task_completed"} {
				events = append(events, Event{Key: fmt.Sprintf("demo:%s:%d", kind, i), Kind: kind, At: at, User: u, Added: Int(140 + i), Deleted: Int(30), Files: Int(8), Commits: Int(3)})
			}
		}
	}
	return events
}
