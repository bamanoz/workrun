package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bamanoz/workrun/internal/assets"
	"github.com/bamanoz/workrun/internal/config"
	"github.com/bamanoz/workrun/internal/domain"
	"github.com/bamanoz/workrun/internal/store"
	"github.com/bamanoz/workrun/internal/workflow"
)

func TestFakeHostHappyPathThroughReviewAndResolution(t *testing.T) {
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
	manifestRaw := []byte("schema_version: 1\nprotocol: \">=1.0 <2.0\"\ntarget_branch: develop\nbranch_template: task/{key}-{slug}\nrequirements:\n  required_roles: [business, functional]\n  traversal: [{kind: description}]\n  max_depth: 1\ntracker_intents:\n  mark_in_review: in-review\n  mark_resolved: resolved\nverification: {policy: discover_from_repository}\nbase_update: {strategy: repository_policy}\nsafety:\n  require_brief_approval: true\n  require_review_approval: true\n")
	manifestHash := config.Hash(manifestRaw)
	if err = st.SaveManifest(ctx, manifestHash, manifestRaw); err != nil {
		t.Fatal(err)
	}
	run, _, err := st.CreateOrResumeRun(ctx, store.CreateRunInput{Provider: "tracker", WorkItemID: "ABC-42", WorkItemKey: "ABC-42", Repository: t.TempDir(), WorkflowName: def.Name, WorkflowHash: hash, ManifestHash: manifestHash})
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireLease(ctx, run.ID, "fake-host", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(st, def)
	eng.Now = func() time.Time { return now }
	caps := domain.CapabilityInventory{ProtocolVersion: domain.ProtocolVersion, Host: "fake", Capabilities: map[string]bool{"tracker.read": true, "requirements.read": true, "vcs.branch": true, "vcs.commit": true, "workspace.worktree": true, "change_request.open": true, "tracker.transition": true, "tracker.comment": true, "change_request.poll": true, "change_request.reply": true}}

	brief := domain.Brief{Problem: "Deliver behavior", Scope: []string{"service"}, NonGoals: []string{}, AcceptanceCriteria: []string{"observable result"}, Constraints: []string{}, RequirementSources: []domain.RequirementSource{{Role: "business", ExternalID: "B-1", Revision: "1"}, {Role: "functional", ExternalID: "F-1", Revision: "2"}}, TestStrategy: "regression test", OpenQuestions: []string{}, ExplicitOverrides: []string{}}
	briefHash, _ := CanonicalHash(brief)
	revisions := []domain.SourceRevision{{Role: "business", ExternalID: "B-1", Revision: "1"}, {Role: "functional", ExternalID: "F-1", Revision: "2"}}
	item := domain.WorkItem{Provider: "tracker", ExternalID: "ABC-42", Key: "ABC-42", Status: "open", Revision: "3"}
	run = complete(t, ctx, eng, *lease, caps, "complete", map[string]any{"work_item": item, "brief": brief, "brief_hash": briefHash, "source_revisions": revisions})
	if run.State != domain.StateAwaitingBriefApproval {
		t.Fatalf("state=%s", run.State)
	}
	if _, err = st.ValidateLease(ctx, run.ID, lease.Token); !errors.Is(err, store.ErrLeaseExpired) {
		t.Fatalf("human gate retained lease: %v", err)
	}
	run, err = eng.Approve(ctx, run.ID, briefHash[:12], "user")
	if err != nil {
		t.Fatal(err)
	}
	lease, err = st.AcquireLease(ctx, run.ID, "fake-host", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	receipt := func(operation, id, head, state, subjectProvider, subjectID string) domain.Receipt {
		return domain.Receipt{Provider: "sourcecontrol", ExternalID: id, Operation: operation, SubjectProvider: subjectProvider, SubjectID: subjectID, ObservedAt: now.Format(time.RFC3339), Head: head, State: state}
	}
	run = complete(t, ctx, eng, *lease, caps, "complete", map[string]any{"workspace": "/tmp/worktree", "branch": "task/ABC-42-deliver", "base_head": "base123", "receipt": receipt("workspace.create", "workspace-1", "base123", "", "local", run.Repository)})
	run = complete(t, ctx, eng, *lease, caps, "complete", map[string]any{"head": "impl123", "summary": "implemented contract"})
	run = complete(t, ctx, eng, *lease, caps, "passed", map[string]any{"head": "impl123", "commands": []any{map[string]any{"argv": []string{"go", "test", "./..."}, "cwd": "/tmp/worktree", "exit_code": 0, "output_hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "head": "impl123"}}, "clean": true})
	run = complete(t, ctx, eng, *lease, caps, "complete", map[string]any{"head": "impl123", "checklist": map[string]bool{"docs": true, "generated": true, "scaffold_removed": true}, "clean": true, "commit_receipt": receipt("vcs.commit", "commit-1", "impl123", "committed", "local", run.Repository)})
	cr := domain.ChangeRequest{Provider: "sourcecontrol", ExternalID: "99", URL: "https://sc.local/review/99", State: "open", Head: "impl123", Base: "develop"}
	run = complete(t, ctx, eng, *lease, caps, "complete", map[string]any{"change_request": cr, "tracker_status": "in_review", "receipts": domain.PublishReceipts{Push: receipt("vcs.push", "p1", "impl123", "", "local", run.Repository), Review: receipt("change_request.open", "r1", "impl123", "open", "sourcecontrol", "99"), Transition: receipt("tracker.transition", "t1", "", "in_review", "tracker", "ABC-42"), Comment: receipt("tracker.comment", "c1", "", "", "tracker", "ABC-42")}, "reconciled": true, "clean": true, "head": "impl123", "source_revisions": revisions, "work_item_revision": "3"})
	if run.State != domain.StateAwaitingReview || run.NextWakeAt == nil {
		t.Fatalf("not waiting for review: %#v", run)
	}
	if _, err = st.ValidateLease(ctx, run.ID, lease.Token); !errors.Is(err, store.ErrLeaseExpired) {
		t.Fatalf("external wait retained lease: %v", err)
	}
	now = run.NextWakeAt.Add(time.Second)
	lease, err = st.AcquireLease(ctx, run.ID, "fake-host", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	run = complete(t, ctx, eng, *lease, caps, "feedback", map[string]any{"change_request_state": "open", "events": []any{map[string]string{"id": "comment-1", "thread_id": "thread-1", "type": "actionable_comment"}}, "cursor": "cursor-1"})
	run = complete(t, ctx, eng, *lease, caps, "complete", reviewPlanEvidence(t))
	if run.State != domain.StateAwaitingReviewApproval {
		t.Fatalf("state=%s", run.State)
	}
	if _, err = st.ValidateLease(ctx, run.ID, lease.Token); !errors.Is(err, store.ErrLeaseExpired) {
		t.Fatalf("review gate retained lease: %v", err)
	}
	run, err = eng.Approve(ctx, run.ID, run.ReviewPlanHash[:12], "user")
	if err != nil {
		t.Fatal(err)
	}
	lease, err = st.AcquireLease(ctx, run.ID, "fake-host", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	run = complete(t, ctx, eng, *lease, caps, "no_change", map[string]any{"change_request_state": "open", "events": []any{}, "cursor": "cursor-1-confirmed"})
	run = complete(t, ctx, eng, *lease, caps, "complete", map[string]any{"head": "fix456", "commands": []any{map[string]any{"argv": []string{"go", "test", "./..."}, "cwd": "/tmp/worktree", "exit_code": 0, "output_hash": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "head": "fix456"}}, "push_receipt": receipt("vcs.push", "push-2", "fix456", "", "local", run.Repository), "thread_receipts": []any{domain.ThreadReceipt{ThreadID: "thread-1", SubjectProvider: "sourcecontrol", SubjectID: "99", ReplyID: "reply-1", Commit: "fix456", Outcome: "fixed", ObservedAt: now.Format(time.RFC3339)}}, "source_revisions": revisions, "work_item_revision": "3", "reconciled": true})
	if run.State != domain.StateAwaitingReview || run.NextWakeAt == nil {
		t.Fatalf("review fixes did not return to wait: %#v", run)
	}
	now = run.NextWakeAt.Add(time.Second)
	lease, err = st.AcquireLease(ctx, run.ID, "fake-host", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	run = complete(t, ctx, eng, *lease, caps, "merged", map[string]any{"change_request_state": "merged", "events": []any{map[string]string{"id": "merge-1", "type": "merged"}}, "cursor": "cursor-2"})
	if run.State != domain.StateMergedPendingResolution {
		t.Fatalf("state=%s", run.State)
	}
	run = complete(t, ctx, eng, *lease, caps, "complete", map[string]any{"merge_receipt": domain.Receipt{Provider: "sourcecontrol", ExternalID: "merge-1", Operation: "change_request.merge", SubjectProvider: "sourcecontrol", SubjectID: "99", ObservedAt: now.Format(time.RFC3339), Head: "fix456", State: "merged"}, "tracker_status": "resolved", "transition_receipt": domain.Receipt{Provider: "tracker", ExternalID: "transition-2", Operation: "tracker.transition", SubjectProvider: "tracker", SubjectID: "ABC-42", ObservedAt: now.Format(time.RFC3339), State: "resolved"}, "reconciled": true})
	if run.State != domain.StateResolved || !run.State.Terminal() {
		t.Fatalf("final run=%#v", run)
	}
	events, err := st.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 14 {
		t.Fatalf("event history too short: %d", len(events))
	}
}

func complete(t *testing.T, ctx context.Context, eng *Engine, lease domain.Lease, caps domain.CapabilityInventory, outcome string, evidence any) *domain.Run {
	t.Helper()
	action, err := eng.Next(ctx, lease.RunID, lease, caps)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	result := domain.ActionResult{ProtocolVersion: domain.ProtocolVersion, RunID: lease.RunID, ActionID: action.ActionID, Nonce: action.Nonce, LeaseToken: lease.Token, Outcome: outcome, Evidence: raw, Summary: action.Intent}
	run, applied, err := eng.Complete(ctx, result)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("completion was not applied")
	}
	return run
}
func reviewPlanEvidence(t *testing.T) map[string]any {
	t.Helper()
	plan := map[string]any{"batch": []string{"thread-1"}, "changes": []string{"handle edge case"}, "responses": []string{"will fix"}}
	hash, err := CanonicalHash(plan)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"review_plan": plan, "review_plan_hash": hash, "thread_ids": []string{"thread-1"}}
}

func TestReviewWakeMovesOutsideHoursToNextWorkday(t *testing.T) {
	eng := &Engine{Now: func() time.Time { return time.Date(2026, 8, 28, 17, 55, 0, 0, time.UTC) }, workZone: time.UTC, workStart: 9 * 60, workEnd: 18 * 60}
	due := eng.nextReviewWake(0)
	want := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	if !due.Equal(want) {
		t.Fatalf("due=%s want=%s", due, want)
	}
}

func TestWorkspaceCloneFallbackAndCapabilityBlock(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	def, err := workflow.Parse(assets.TrackedChangeWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	hash := workflow.Hash(assets.TrackedChangeWorkflow)
	if err = st.SaveWorkflow(ctx, hash, def.Name, def.Protocol, assets.TrackedChangeWorkflow); err != nil {
		t.Fatal(err)
	}
	run, _, err := st.CreateOrResumeRun(ctx, store.CreateRunInput{Provider: "tracker", WorkItemID: "CAP-1", WorkItemKey: "CAP-1", Repository: t.TempDir(), WorkflowName: def.Name, WorkflowHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	run.State = domain.StatePreparingWorkspace
	if err = st.UpdateRun(ctx, run, "test_setup", "", nil); err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireLease(ctx, run.ID, "host", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(st, def)
	cloneCaps := domain.CapabilityInventory{Capabilities: map[string]bool{"vcs.branch": true, "workspace.clone": true}}
	action, err := eng.Next(ctx, run.ID, *lease, cloneCaps)
	if err != nil {
		t.Fatal(err)
	}
	var inputs map[string]any
	if err = json.Unmarshal(action.Inputs, &inputs); err != nil {
		t.Fatal(err)
	}
	if inputs["workspace_mode"] != "clone" {
		t.Fatalf("mode=%v", inputs["workspace_mode"])
	}
	if err = st.FailPendingAction(ctx, run.ID, action.ActionID, lease.Token, domain.ActionResult{RunID: run.ID, ActionID: action.ActionID, LeaseToken: lease.Token}); err != nil {
		t.Fatal(err)
	}
	// A fresh run with neither isolation capability must be persisted as blocked_capability.
	run2, _, err := st.CreateOrResumeRun(ctx, store.CreateRunInput{Provider: "tracker", WorkItemID: "CAP-2", WorkItemKey: "CAP-2", Repository: t.TempDir(), WorkflowName: def.Name, WorkflowHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	run2.State = domain.StatePreparingWorkspace
	if err = st.UpdateRun(ctx, run2, "test_setup", "", nil); err != nil {
		t.Fatal(err)
	}
	lease2, err := st.AcquireLease(ctx, run2.ID, "host", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = eng.Next(ctx, run2.ID, *lease2, domain.CapabilityInventory{Capabilities: map[string]bool{"vcs.branch": true}})
	if !errors.Is(err, ErrCapability) {
		t.Fatalf("expected capability error, got %v", err)
	}
	stored, err := st.GetRun(ctx, run2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.StateBlocked || stored.WaitKind != domain.WaitCapability {
		t.Fatalf("run not capability-blocked: %#v", stored)
	}
}

func TestMissingRequirementBlocksWithProvisionalBrief(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	def, err := workflow.Parse(assets.TrackedChangeWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	hash := workflow.Hash(assets.TrackedChangeWorkflow)
	if err = st.SaveWorkflow(ctx, hash, def.Name, def.Protocol, assets.TrackedChangeWorkflow); err != nil {
		t.Fatal(err)
	}
	manifest := []byte("schema_version: 1\nprotocol: \">=1.0 <2.0\"\ntarget_branch: develop\nbranch_template: task/{key}-{slug}\nrequirements:\n  required_roles: [business, functional]\n  traversal: [{kind: description}]\n  max_depth: 1\ntracker_intents: {mark_in_review: review, mark_resolved: resolved}\nverification: {policy: discover}\nbase_update: {strategy: repository}\nsafety: {require_brief_approval: true, require_review_approval: true}\n")
	manifestHash := config.Hash(manifest)
	if err = st.SaveManifest(ctx, manifestHash, manifest); err != nil {
		t.Fatal(err)
	}
	run, _, err := st.CreateOrResumeRun(ctx, store.CreateRunInput{Provider: "tracker", WorkItemID: "MISS-1", WorkItemKey: "MISS-1", Repository: t.TempDir(), WorkflowName: def.Name, WorkflowHash: hash, ManifestHash: manifestHash})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireLease(ctx, run.ID, "host", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(st, def)
	caps := domain.CapabilityInventory{Capabilities: map[string]bool{"tracker.read": true, "requirements.read": true}}
	brief := domain.Brief{Problem: "Implement requested behavior", Scope: []string{"service"}, NonGoals: []string{}, AcceptanceCriteria: []string{"observable result"}, Constraints: []string{}, RequirementSources: []domain.RequirementSource{{Role: "business", ExternalID: "B-1", Revision: "1"}}, TestStrategy: "add regression test", OpenQuestions: []string{"Where is the functional specification?"}, ExplicitOverrides: []string{}}
	briefHash, _ := CanonicalHash(brief)
	item := domain.WorkItem{Provider: "tracker", ExternalID: "MISS-1", Key: "MISS-1", Status: "open", Revision: "1"}
	run = complete(t, ctx, eng, *lease, caps, "needs_input", map[string]any{"work_item": item, "brief": brief, "brief_hash": briefHash, "source_revisions": []domain.SourceRevision{{Role: "business", ExternalID: "B-1", Revision: "1"}}, "missing_roles": []string{"functional"}})
	if run.State != domain.StateBlocked || run.WaitKind != domain.WaitHuman {
		t.Fatalf("missing requirement did not block: %#v", run)
	}
	if _, err = st.ValidateLease(ctx, run.ID, lease.Token); !errors.Is(err, store.ErrLeaseExpired) {
		t.Fatalf("blocked run retained lease: %v", err)
	}
}
