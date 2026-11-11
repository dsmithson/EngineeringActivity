package facts

import (
	"testing"
	"time"
)

func revs(changes []struct{ state, user string }) []Revision {
	rs := []Revision{}
	for i, c := range changes {
		f := map[string]any{"System.State": c.state, "System.CreatedDate": "2026-09-18T01:00:00Z",
			"System.ChangedDate": time.Date(2026, 9, 18, i+1, 0, 0, 0, time.UTC).Format(time.RFC3339), "System.WorkItemType": "Task", "System.Title": "T"}
		if c.user != "" {
			f["System.AssignedTo"] = map[string]any{"id": c.user, "displayName": c.user, "uniqueName": c.user + "@example.test"}
		}
		rs = append(rs, Revision{Rev: i + 1, Fields: f})
	}
	return rs
}

func TestHistoricalAssigneeAndReopen(t *testing.T) {
	done := ParseCompletionStates("Ready for QA Test Build, ready to test ,closed")
	rs := revs([]struct{ state, user string }{{"Active", "alice"}, {"Ready for QA Test Build", "bob"}, {"Ready to Test", "carol"}, {"Closed", "carol"}, {"Active", "alice"}, {"Closed", "alice"}})
	w, es, err := DeriveWorkItem("org", "proj", 42, rs, done)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 || es[0].Type != "created" || es[1].Type != "completed" || es[2].Type != "completed" {
		t.Fatalf("wanted creation and two completions, got %+v", es)
	}
	for i, want := range []string{"alice", "bob", "alice"} {
		if es[i].Identity.ID != want {
			t.Errorf("event %d attributed to %s instead of %s", i, es[i].Identity.ID, want)
		}
	}
	if es[1].FromState != "Active" || es[1].ToState != "Ready for QA Test Build" || es[2].Revision != 6 {
		t.Fatalf("transition details %+v", es)
	}
	if w.LastRevision != 6 || w.CurrentState != "Closed" || w.Type != "Task" {
		t.Fatalf("work item summary %+v", w)
	}
	if _, _, err := DeriveWorkItem("org", "proj", 42, rs[1:], done); err == nil {
		t.Error("incomplete history must fail")
	}
}

func TestCreatedDirectlyInCompletionStateCountsOnce(t *testing.T) {
	done := ParseCompletionStates("closed")
	_, es, err := DeriveWorkItem("org", "proj", 1, revs([]struct{ state, user string }{{"Closed", ""}, {"Closed", "bob"}}), done)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || es[1].Type != "completed" || es[1].Revision != 1 || es[1].Identity.ID != "unassigned" {
		t.Fatalf("events %+v", es)
	}
}

func TestIdentityFromRef(t *testing.T) {
	if IdentityFromRef(nil).ID != "unassigned" {
		t.Fatal("nil assignment")
	}
	if IdentityFromRef(map[string]any{}).ID != "unknown" || IdentityFromRef("legacy string").ID != "unknown" {
		t.Fatal("malformed assignment should be unknown")
	}
	i := IdentityFromRef(map[string]any{"id": "ABC", "displayName": "Ann", "uniqueName": "Ann@Example.Test"})
	if i.ID != "abc" || i.AliasEmail() != "ann@example.test" || i.IsService {
		t.Fatalf("identity %+v", i)
	}
	svc := NewADOIdentity("x", "Platform Build Service (acme)", "Build\abc")
	if !svc.IsService || svc.AliasEmail() != "" {
		t.Fatal("service identity")
	}
}

func TestMergeStatsAreExcluded(t *testing.T) {
	c := Commit{ParentCount: 2, Files: []FileChange{{Path: "a", Additions: Int(1), Deletions: Int(1)}}}
	c.ApplyStats()
	if c.StatsStatus != StatsMerge || c.Files != nil || c.Additions != nil {
		t.Fatalf("merge %+v", c)
	}
}
