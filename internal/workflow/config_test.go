package workflow

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bamanoz/workrun/internal/assets"
)

func TestEmbeddedWorkflowIsValid(t *testing.T) {
	def, err := Parse(assets.TrackedChangeWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if def.Initial != "discovering_context" {
		t.Fatalf("initial=%q", def.Initial)
	}
	if !def.States["resolved"].Terminal {
		t.Fatal("resolved must be terminal")
	}
}

func TestWorkflowRejectsUnknownFields(t *testing.T) {
	raw := strings.Replace(string(assets.TrackedChangeWorkflow), "name: tracked-change", "name: tracked-change\nunknown: true", 1)
	if _, err := Parse([]byte(raw)); err == nil {
		t.Fatal("expected strict unknown-field error")
	}
}

func TestWorkflowRejectsUnknownTargets(t *testing.T) {
	raw := strings.Replace(string(assets.TrackedChangeWorkflow), "complete: awaiting_brief_approval", "complete: nowhere", 1)
	if _, err := Parse([]byte(raw)); err == nil {
		t.Fatal("expected unknown target error")
	}
}

func TestEveryNonExplicitStateIsReachable(t *testing.T) {
	def, err := Parse(assets.TrackedChangeWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	queue := []string{def.Initial}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		for _, target := range def.States[name].On {
			queue = append(queue, target)
		}
	}
	for name := range def.States {
		if name == "reconciling" || name == "paused" || name == "cancelled" || name == "superseded" || name == "failed_permanent" {
			continue
		}
		if !seen[name] {
			t.Errorf("state %q is unreachable", name)
		}
	}
}

func TestCanonicalFSMModelGolden(t *testing.T) {
	def, err := Parse(assets.TrackedChangeWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Hash(assets.TrackedChangeWorkflow), "7af4caf327cb712e2534d2f6d1938ab304cc4534d7331fc61e83c697da42c1c5"; got != want {
		t.Fatalf("workflow golden changed: %s", got)
	}
	expected := map[string]map[string]string{
		"reconciling": {"complete": "discovering_context", "changed": "discovering_context", "blocked": "blocked"}, "discovering_context": {"complete": "awaiting_brief_approval", "needs_input": "blocked"}, "awaiting_brief_approval": {"approved": "preparing_workspace", "changed": "discovering_context"}, "brief_stale": {"complete": "awaiting_brief_approval", "needs_input": "blocked"}, "preparing_workspace": {"complete": "implementing", "blocked": "blocked"}, "implementing": {"complete": "verifying", "scope_changed": "brief_stale", "blocked": "blocked"}, "verifying": {"passed": "finalizing", "failed_fixable": "implementing", "blocked": "blocked"}, "finalizing": {"complete": "publishing", "needs_changes": "implementing", "blocked": "blocked"}, "publishing": {"complete": "awaiting_review", "blocked": "blocked"}, "awaiting_review": {"no_change": "awaiting_review", "feedback": "analyzing_review", "merged": "merged_pending_resolution", "closed": "blocked"}, "analyzing_review": {"complete": "awaiting_review_plan_approval", "no_action": "awaiting_review", "blocked": "blocked"}, "awaiting_review_plan_approval": {"approved": "confirming_review_plan", "changed": "analyzing_review"}, "confirming_review_plan": {"no_change": "addressing_review", "feedback_independent": "addressing_review", "feedback_overlap": "analyzing_review", "merged": "merged_pending_resolution", "closed": "blocked"}, "addressing_review": {"complete": "awaiting_review", "scope_changed": "brief_stale", "blocked": "blocked"}, "merged_pending_resolution": {"complete": "resolved", "blocked": "blocked"}, "blocked": {"resume": "discovering_context"}, "paused": {"resume": "discovering_context"}, "resolved": {}, "cancelled": {}, "superseded": {}, "failed_permanent": {}}
	got := map[string]map[string]string{}
	for name, state := range def.States {
		if state.On == nil {
			got[name] = map[string]string{}
		} else {
			got[name] = state.On
		}
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("FSM model changed\ngot: %#v\nwant: %#v", got, expected)
	}
	if def.States["awaiting_brief_approval"].Gate != "approve_brief" || def.States["awaiting_review_plan_approval"].Gate != "approve_review_plan" {
		t.Fatal("mandatory gates changed")
	}
}
