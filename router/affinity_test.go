package router

import (
	"testing"
	"time"
)

func TestWorkflowAffinityIsExplicitAndIndependentOfSessions(t *testing.T) {
	store := NewWorkflowAffinityStore(time.Hour, 10)
	store.Remember("workflow-a", "anthropic/sonnet")

	for _, sessionID := range []string{"subagent-1", "subagent-2"} {
		_ = sessionID // session identity is intentionally not an input to affinity.
		model, ok := store.Preferred("workflow-a")
		if !ok || model != "anthropic/sonnet" {
			t.Fatalf("affinity = %q, %v", model, ok)
		}
	}
	if _, ok := store.Preferred(""); ok {
		t.Fatal("empty workflow ID acquired inferred affinity")
	}
	if _, ok := store.Preferred("workflow-b"); ok {
		t.Fatal("unrelated workflow shared affinity")
	}
}

func TestWorkflowAffinityExpires(t *testing.T) {
	now := time.Unix(100, 0)
	store := NewWorkflowAffinityStore(time.Minute, 10)
	store.now = func() time.Time { return now }
	store.Remember("workflow-a", "model-a")
	now = now.Add(time.Minute)
	if _, ok := store.Preferred("workflow-a"); ok {
		t.Fatal("expired affinity returned")
	}
}

func TestWorkflowAffinityEvictsOldestAtCapacity(t *testing.T) {
	now := time.Unix(100, 0)
	store := NewWorkflowAffinityStore(time.Hour, 2)
	store.now = func() time.Time { return now }
	store.Remember("workflow-a", "model-a")
	now = now.Add(time.Second)
	store.Remember("workflow-b", "model-b")
	now = now.Add(time.Second)
	store.Remember("workflow-c", "model-c")

	if _, ok := store.Preferred("workflow-a"); ok {
		t.Fatal("oldest affinity was not evicted")
	}
	for _, id := range []string{"workflow-b", "workflow-c"} {
		if _, ok := store.Preferred(id); !ok {
			t.Fatalf("affinity %q unexpectedly missing", id)
		}
	}
}

func TestWorkflowAffinityEmptyValuesDoNotMutateState(t *testing.T) {
	store := NewWorkflowAffinityStore(time.Hour, 10)
	store.Remember("workflow-a", "model-a")
	store.Remember("workflow-a", "")
	store.Remember("", "model-b")
	model, ok := store.Preferred("workflow-a")
	if !ok || model != "model-a" {
		t.Fatalf("valid affinity changed to %q, %v", model, ok)
	}
}
