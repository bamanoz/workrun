package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bamanoz/workrun/internal/assets"
	"github.com/bamanoz/workrun/internal/domain"
	"github.com/bamanoz/workrun/internal/store"
	"github.com/bamanoz/workrun/internal/workflow"
)

func TestEvidenceIsStrictAndRedacted(t *testing.T) {
	eng := &Engine{}
	run := &domain.Run{}
	if _, _, err := eng.validateAndNormalizeEvidence(context.Background(), run, "implement_change", "complete", json.RawMessage(`{"head":"abc","summary":"ok","unknown":true}`)); !errors.Is(err, ErrBadEvidence) {
		t.Fatalf("unknown field accepted: %v", err)
	}
	normalized, _, err := eng.validateAndNormalizeEvidence(context.Background(), run, "implement_change", "complete", json.RawMessage(`{"head":"abc","summary":"authorization: Bearer abcdefghijklmnop"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(normalized) != `{"head":"abc","summary":"[REDACTED]"}` {
		t.Fatalf("secret not redacted: %s", normalized)
	}
	standalone, _, err := eng.validateAndNormalizeEvidence(context.Background(), run, "implement_change", "complete", json.RawMessage(`{"head":"abc","summary":"ghp_abcdefghijklmnopqrstuvwxyz123456"}`))
	if err != nil || bytes.Contains(standalone, []byte("ghp_")) {
		t.Fatalf("standalone token not redacted: %s %v", standalone, err)
	}
}

func TestHistoryEventDataKeepsReceiptsWithoutContent(t *testing.T) {
	raw := json.RawMessage(`{"outcome":"complete","evidence":{"brief":{"problem":"private requirement"},"receipts":{"push":{"provider":"vcs","external_id":"1"}}},"summary":"authorization: Bearer abcdefghijklmnop"}`)
	safe := HistoryEventData(raw)
	if bytes.Contains(safe, []byte("private requirement")) || bytes.Contains(safe, []byte("abcdefghijklmnop")) {
		t.Fatalf("history leaked content: %s", safe)
	}
	if !bytes.Contains(safe, []byte(`"receipts"`)) {
		t.Fatalf("history lost receipts: %s", safe)
	}
}

func TestPublishEvidenceDetectsApprovedRequirementDrift(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	receipt := func(operation string) domain.Receipt {
		r := domain.Receipt{Provider: "sourcecontrol", ExternalID: operation, Operation: operation, ObservedAt: now, Head: "head"}
		switch operation {
		case "vcs.push":
			r.SubjectProvider = "local"
			r.SubjectID = "/repo"
		case "change_request.open":
			r.SubjectProvider = "sourcecontrol"
			r.SubjectID = "1"
			r.State = "open"
		default:
			r.SubjectProvider = "tracker"
			r.SubjectID = "T-1"
		}
		if operation == "tracker.transition" {
			r.State = "in_review"
		}
		return r
	}
	value := &domain.PublishEvidence{
		ChangeRequest: domain.ChangeRequest{Provider: "sourcecontrol", ExternalID: "1", URL: "https://sc.local/review/1", State: "open", Head: "head", Base: "develop"},
		TrackerStatus: "in_review", Head: "head", Clean: true, Reconciled: true,
		SourceRevisions: []domain.SourceRevision{{Role: "business", ExternalID: "B-1", Revision: "2"}},
		Receipts:        domain.PublishReceipts{Push: receipt("vcs.push"), Review: receipt("change_request.open"), Transition: receipt("tracker.transition"), Comment: receipt("tracker.comment")},
	}
	run := &domain.Run{Repository: "/repo", WorkItemProvider: "tracker", WorkItemID: "T-1", Head: "head", VerifiedHead: "head", SourceRevisions: []domain.SourceRevision{{Role: "business", ExternalID: "B-1", Revision: "1"}}}
	if err := (&Engine{}).validateTypedEvidence(context.Background(), run, "complete", value); !errors.Is(err, ErrRequirementDrift) {
		t.Fatalf("drift accepted: %v", err)
	}
	value.SourceRevisions = run.SourceRevisions
	value.WorkItemRevision = "rev"
	run.WorkItem = &domain.WorkItem{Revision: "rev"}
	run.Head = "verified"
	run.VerifiedHead = "verified"
	if err := (&Engine{}).validateTypedEvidence(context.Background(), run, "complete", value); err == nil || !strings.Contains(err.Error(), "fresh successful verification") {
		t.Fatalf("unverified publication accepted: %v", err)
	}
}

func TestReviewConfirmationInvalidatesOnlyOverlappingPlan(t *testing.T) {
	eng := &Engine{}
	approved := &domain.Run{ReviewPlanHash: "plan", ReviewPlanThreadIDs: []string{"old-thread"}}
	overlap := map[string]json.RawMessage{}
	raw, _ := json.Marshal(domain.PollEvidence{ChangeRequestState: "open", Cursor: "2", Overlap: "overlap", Events: []domain.ReviewEvent{{ID: "new-comment", ThreadID: "new-thread", Type: "actionable_comment"}, {ID: "new-reply", ThreadID: "new-thread", Type: "actionable_comment"}}})
	_ = json.Unmarshal(raw, &overlap)
	if _, err := eng.processReviewEvidence(approved, "feedback_overlap", overlap); err != nil {
		t.Fatal(err)
	}
	if approved.ReviewPlanHash != "" || !sameStrings(approved.PendingReviewThreadIDs, []string{"old-thread", "new-thread"}) {
		t.Fatalf("overlap did not invalidate and rebatch by thread: %#v", approved)
	}

	independent := &domain.Run{ReviewPlanHash: "plan", ReviewPlanThreadIDs: []string{"old-thread"}}
	raw, _ = json.Marshal(domain.PollEvidence{ChangeRequestState: "open", Cursor: "2", Overlap: "independent", Events: []domain.ReviewEvent{{ID: "new-comment", ThreadID: "new-thread", Type: "actionable_comment"}}})
	_ = json.Unmarshal(raw, &overlap)
	if _, err := eng.processReviewEvidence(independent, "feedback_independent", overlap); err != nil {
		t.Fatal(err)
	}
	if independent.ReviewPlanHash != "plan" || !sameStrings(independent.PendingReviewThreadIDs, []string{"new-thread"}) {
		t.Fatalf("independent feedback invalidated approval: %#v", independent)
	}
}

func TestTransientReadFailureSchedulesPersistedRetry(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })
	def, err := workflow.Parse(assets.TrackedChangeWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	hash := workflow.Hash(assets.TrackedChangeWorkflow)
	if err = st.SaveWorkflow(ctx, hash, def.Name, def.Protocol, assets.TrackedChangeWorkflow); err != nil {
		t.Fatal(err)
	}
	run, _, err := st.CreateOrResumeRun(ctx, store.CreateRunInput{Provider: "tracker", WorkItemID: "RETRY-1", WorkItemKey: "RETRY-1", Repository: t.TempDir(), WorkflowName: def.Name, WorkflowHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireLease(ctx, run.ID, "host", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(st, def)
	eng.Now = func() time.Time { return now }
	action, err := eng.Next(ctx, run.ID, *lease, domain.CapabilityInventory{Capabilities: map[string]bool{"tracker.read": true, "requirements.read": true}})
	if err != nil {
		t.Fatal(err)
	}
	run, err = eng.Fail(ctx, domain.ActionResult{ProtocolVersion: domain.ProtocolVersion, RunID: run.ID, ActionID: action.ActionID, Nonce: action.Nonce, LeaseToken: lease.Token}, "temporary timeout", "transient")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.StateDiscoveringContext || run.RetryAttempt != 1 || run.NextWakeAt == nil || !run.NextWakeAt.Equal(now.Add(10*time.Second)) {
		t.Fatalf("retry not persisted: %#v", run)
	}
	retryLease, err := st.AcquireLease(ctx, run.ID, "host", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = eng.Next(ctx, run.ID, *retryLease, domain.CapabilityInventory{}); !errors.Is(err, ErrWaiting) {
		t.Fatalf("retry schedule ignored: %v", err)
	}
}

func TestRemoteWriteBoundariesReconcileAfterLeaseLoss(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })
	def, err := workflow.Parse(assets.TrackedChangeWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	hash := workflow.Hash(assets.TrackedChangeWorkflow)
	if err = st.SaveWorkflow(ctx, hash, def.Name, def.Protocol, assets.TrackedChangeWorkflow); err != nil {
		t.Fatal(err)
	}
	eng := New(st, def)
	eng.Now = func() time.Time { return now }
	cases := []struct {
		state domain.State
		caps  map[string]bool
	}{{domain.StatePublishing, map[string]bool{"change_request.open": true, "tracker.transition": true, "tracker.comment": true}}, {domain.StateAddressingReview, map[string]bool{"tracker.read": true, "requirements.read": true, "change_request.reply": true}}, {domain.StateMergedPendingResolution, map[string]bool{"change_request.poll": true, "tracker.transition": true}}}
	for index, tc := range cases {
		run, _, createErr := st.CreateOrResumeRun(ctx, store.CreateRunInput{Provider: "tracker", WorkItemID: fmt.Sprintf("WRITE-%d", index), WorkItemKey: fmt.Sprintf("WRITE-%d", index), Repository: t.TempDir(), WorkflowName: def.Name, WorkflowHash: hash})
		if createErr != nil {
			t.Fatal(createErr)
		}
		run.State = tc.state
		if updateErr := st.UpdateRun(ctx, run, "test_setup", "", nil); updateErr != nil {
			t.Fatal(updateErr)
		}
		firstLease, leaseErr := st.AcquireLease(ctx, run.ID, "host-1", time.Minute)
		if leaseErr != nil {
			t.Fatal(leaseErr)
		}
		first, nextErr := eng.Next(ctx, run.ID, *firstLease, domain.CapabilityInventory{Capabilities: tc.caps})
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		now = now.Add(2 * time.Minute)
		secondLease, leaseErr := st.AcquireLease(ctx, run.ID, "host-2", time.Minute)
		if leaseErr != nil {
			t.Fatal(leaseErr)
		}
		reconciled, nextErr := eng.Next(ctx, run.ID, *secondLease, domain.CapabilityInventory{Capabilities: tc.caps})
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if reconciled.ActionID != first.ActionID || reconciled.Nonce != first.Nonce || reconciled.RetryClass != "reconcile_first" {
			t.Fatalf("%s write was not reconciled: first=%#v retry=%#v", tc.state, first, reconciled)
		}
	}
}
