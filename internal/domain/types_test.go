package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOnlyDeclaredOutcomesAreTerminal(t *testing.T) {
	terminal := map[State]bool{StateResolved: true, StateCancelled: true, StateSuperseded: true, StateFailedPermanent: true}
	states := []State{StateDiscoveringContext, StateReconciling, StateAwaitingBriefApproval, StateBriefStale, StatePreparingWorkspace, StateImplementing, StateVerifying, StateFinalizing, StatePublishing, StateAwaitingReview, StateAnalyzingReview, StateAwaitingReviewApproval, StateConfirmingReviewPlan, StateAddressingReview, StateMergedPendingResolution, StateBlocked, StatePaused, StateResolved, StateCancelled, StateSuperseded, StateFailedPermanent}
	for _, state := range states {
		if state.Terminal() != terminal[state] {
			t.Errorf("state %s Terminal()=%v", state, state.Terminal())
		}
	}
}

func TestActionEnvelopeGoldenJSON(t *testing.T) {
	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	value := ActionEnvelope{ProtocolVersion: "1.0", SchemaVersion: 1, RunID: "run_1", ActionID: "act_1", Nonce: "nonce_1", Lease: Lease{RunID: "run_1", Token: "lease_1", Owner: "agent", ExpiresAt: deadline}, Type: "verify_change", Intent: "verify", Inputs: json.RawMessage(`{"repository":"/repo"}`), RequiredEvidence: EvidenceRequirement{Kind: "verification", RequiredFields: []string{"command", "exit_code", "head"}}, RetryClass: "safe", Deadline: deadline}
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol_version":"1.0","schema_version":1,"run_id":"run_1","action_id":"act_1","nonce":"nonce_1","lease":{"run_id":"run_1","token":"lease_1","owner":"agent","expires_at":"2026-01-02T03:04:05Z"},"type":"verify_change","intent":"verify","inputs":{"repository":"/repo"},"required_evidence":{"kind":"verification","required_fields":["command","exit_code","head"],"reconcile":false},"retry_class":"safe","deadline":"2026-01-02T03:04:05Z"}`
	if string(got) != want {
		t.Fatalf("protocol changed\ngot:  %s\nwant: %s", got, want)
	}
}
