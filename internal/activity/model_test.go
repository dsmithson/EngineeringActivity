package activity

import (
	"testing"
	"time"
)

func TestHourlyWindowDedupAndFilter(t *testing.T) {
	now := time.Date(2026, 9, 18, 15, 23, 0, 0, time.UTC)
	start, _ := Window(now)
	a := Identity{ID: "a", Name: "Alice"}
	b := Identity{ID: "b", Name: "Bob"}
	es := []Event{{Key: "1", Kind: "commit", At: start, User: a, Added: Int(12), Deleted: Int(3), Files: Int(1)}, {Key: "1", Kind: "commit", At: start, User: a, Added: Int(12)}, {Key: "2", Kind: "commit", At: now, User: b}, {Key: "3", Kind: "task_created", At: start.Add(-time.Second), User: a}, {Key: "4", Kind: "commit", At: now.Add(time.Second), User: a}}
	r := Aggregate(es, now, "")
	if len(r.Buckets) != 168 || r.Buckets[0].Commits != 1 || r.Buckets[167].Commits != 1 || r.Buckets[0].Added != 12 {
		t.Fatalf("incorrect aggregation: %+v %+v", r.Buckets[0], r.Buckets[167])
	}
	if r.Buckets[167].CommitUnknown != 1 {
		t.Error("missing stats must be marked")
	}
	if r.Buckets[1].At.Sub(r.Buckets[0].At) != time.Hour {
		t.Error("not consecutive")
	}
	filtered := Aggregate(es, now, "a")
	if filtered.Buckets[167].Commits != 0 || len(filtered.Identities) != 2 {
		t.Error("filter must preserve identity choices while filtering totals")
	}
}
