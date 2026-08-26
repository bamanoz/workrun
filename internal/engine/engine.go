package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bamanoz/workrun/internal/config"
	"github.com/bamanoz/workrun/internal/domain"
	"github.com/bamanoz/workrun/internal/store"
	"github.com/bamanoz/workrun/internal/workflow"
)

var (
	ErrHumanGate        = errors.New("run is waiting for human approval")
	ErrWaiting          = errors.New("run is waiting for an external event")
	ErrTerminal         = errors.New("run is terminal")
	ErrCapability       = errors.New("required host capability is missing")
	ErrRequirementDrift = errors.New("requirement sources changed")
	ErrBadEvidence      = errors.New("evidence does not satisfy the action contract")
)

type Engine struct {
	Store     *store.Store
	Workflow  *workflow.Definition
	Now       func() time.Time
	User      *config.User
	workZone  *time.Location
	workStart int
	workEnd   int
}

func New(st *store.Store, def *workflow.Definition) *Engine {
	return &Engine{Store: st, Workflow: def, Now: func() time.Time { return time.Now().UTC() }, workZone: time.Local, workStart: 9 * 60, workEnd: 18 * 60}
}

func (e *Engine) ConfigureWorkHours(zone *time.Location, startHour, startMinute, endHour, endMinute int) error {
	start, end := startHour*60+startMinute, endHour*60+endMinute
	if zone == nil || start < 0 || end > 24*60 || start >= end {
		return errors.New("invalid work-hours configuration")
	}
	e.workZone = zone
	e.workStart = start
	e.workEnd = end
	return nil
}

func CanonicalHash(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (e *Engine) Next(ctx context.Context, runID string, lease domain.Lease, caps domain.CapabilityInventory) (*domain.ActionEnvelope, error) {
	if _, err := e.Store.ValidateLease(ctx, runID, lease.Token); err != nil {
		return nil, err
	}
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	state, ok := e.Workflow.States[string(run.State)]
	if !ok {
		return nil, fmt.Errorf("workflow state %q is missing", run.State)
	}
	if run.NextWakeAt != nil && run.NextWakeAt.After(e.Now()) {
		return nil, fmt.Errorf("%w until %s: %s", ErrWaiting, run.NextWakeAt.Format(time.RFC3339), run.WaitReason)
	}
	if state.Terminal {
		return nil, ErrTerminal
	}
	if state.Gate != "" {
		return nil, fmt.Errorf("%w: %s", ErrHumanGate, state.Gate)
	}
	if state.Wait != "" {
		return nil, fmt.Errorf("%w: %s", ErrWaiting, run.WaitReason)
	}
	if state.Action == "" {
		return nil, fmt.Errorf("state %q has no action", run.State)
	}

	workItem := any(map[string]string{"provider": run.WorkItemProvider, "id": run.WorkItemID, "key": run.WorkItemKey})
	if run.WorkItem != nil {
		workItem = run.WorkItem
	}
	inputs := map[string]any{"repository": run.Repository, "work_item": workItem}
	if run.ManifestHash != "" {
		raw, err := e.Store.GetManifest(ctx, run.ManifestHash)
		if err != nil {
			return nil, fmt.Errorf("load pinned repository manifest: %w", err)
		}
		inputs["repository_manifest_yaml"] = string(raw)
		inputs["repository_manifest_hash"] = run.ManifestHash
	}
	if run.WorkItemProvider == "workrun-evolution" {
		proposal, proposalErr := e.Store.GetProposal(ctx, run.WorkItemID)
		if proposalErr != nil {
			return nil, fmt.Errorf("load workflow proposal: %w", proposalErr)
		}
		inputs["workflow_proposal"] = proposal
	}
	missing := missingCapabilities(state.RequiredCaps, caps.Capabilities)
	if state.Action == "prepare_workspace" {
		mode := "worktree"
		if !caps.Capabilities["workspace.worktree"] {
			if caps.Capabilities["workspace.clone"] {
				mode = "clone"
			} else {
				missing = append(missing, "workspace.worktree|workspace.clone")
			}
		}
		inputs["workspace_mode"] = mode
	}
	if len(missing) > 0 {
		reason := "missing capabilities: " + strings.Join(missing, ", ")
		run.ResumeState = run.State
		run.State = domain.StateBlocked
		run.WaitKind = domain.WaitCapability
		run.WaitReason = reason
		run.NextWakeAt = nil
		if err := e.Store.UpdateRun(ctx, run, "capability_blocked", "", map[string]any{"missing": missing}); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", ErrCapability, reason)
	}
	if run.BriefHash != "" {
		inputs["brief_hash"] = run.BriefHash
		brief, err := e.Store.GetBrief(ctx, run.ID, run.BriefHash)
		if err != nil {
			return nil, fmt.Errorf("load pinned execution brief: %w", err)
		}
		inputs["execution_brief"] = brief
	}
	if run.ReviewPlanHash != "" {
		inputs["review_plan_hash"] = run.ReviewPlanHash
		plan, err := e.Store.GetArtifact(ctx, run.ID, "review_plan", run.ReviewPlanHash)
		if err != nil {
			return nil, fmt.Errorf("load pinned review plan: %w", err)
		}
		inputs["review_plan"] = json.RawMessage(plan)
		inputs["review_plan_thread_ids"] = run.ReviewPlanThreadIDs
	}
	if len(run.PendingReviewThreadIDs) > 0 {
		inputs["pending_review_thread_ids"] = run.PendingReviewThreadIDs
	}
	if run.Workspace != "" {
		inputs["workspace"] = run.Workspace
	}
	if run.Branch != "" {
		inputs["branch"] = run.Branch
	}
	if run.Head != "" {
		inputs["head"] = run.Head
	}
	if run.ChangeRequest != nil {
		inputs["change_request"] = run.ChangeRequest
	}
	if cursor, err := e.Store.GetCursor(ctx, run.ID, "change_request.poll"); err == nil {
		inputs["cursor"] = cursor
	}

	outcomes := make([]string, 0, len(state.On))
	for outcome := range state.On {
		outcomes = append(outcomes, outcome)
	}
	sort.Strings(outcomes)
	if pending, err := e.Store.PendingAction(ctx, run.ID); err == nil {
		pending.Lease = lease
		pending.Deadline = lease.ExpiresAt
		pending.RetryClass = "reconcile_first"
		pending.AllowedOutcomes = outcomes
		pending.RequiredEvidence = evidenceFor(state.Action)
		if err := e.Store.RebindPendingAction(ctx, *pending); err != nil {
			return nil, err
		}
		return pending, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	actionID, err := store.NewID("action")
	if err != nil {
		return nil, err
	}
	nonce, err := store.NewID("nonce")
	if err != nil {
		return nil, err
	}
	rawInputs, _ := json.Marshal(inputs)
	action := domain.ActionEnvelope{ProtocolVersion: domain.ProtocolVersion, SchemaVersion: domain.SchemaVersion, RunID: run.ID, ActionID: actionID, Nonce: nonce, Lease: lease, Type: state.Action, AllowedOutcomes: outcomes, Intent: intentFor(state.Action), Inputs: rawInputs, RequiredEvidence: evidenceFor(state.Action), RetryClass: retryClass(e.Workflow, state), Deadline: lease.ExpiresAt}
	if err := e.Store.CreateAction(ctx, action); err != nil {
		return nil, err
	}
	return &action, nil
}

func (e *Engine) Complete(ctx context.Context, result domain.ActionResult) (*domain.Run, bool, error) {
	if result.ProtocolVersion != domain.ProtocolVersion {
		return nil, false, fmt.Errorf("protocol mismatch: got %q", result.ProtocolVersion)
	}
	action, err := e.Store.PendingAction(ctx, result.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			run, getErr := e.Store.GetRun(ctx, result.RunID)
			return run, false, getErr
		}
		return nil, false, err
	}
	if action.ActionID != result.ActionID || action.Nonce != result.Nonce {
		return nil, false, store.ErrConflict
	}
	run, err := e.Store.GetRun(ctx, result.RunID)
	if err != nil {
		return nil, false, err
	}
	normalized, evidence, err := e.validateAndNormalizeEvidence(ctx, run, action.Type, result.Outcome, result.Evidence)
	if errors.Is(err, ErrRequirementDrift) {
		result.Evidence = nil
		result.Summary = "requirement drift detected"
		run.ResumeState = run.State
		run.State = domain.StateBriefStale
		run.WaitKind = domain.WaitHuman
		run.WaitReason = err.Error()
		run.NextWakeAt = nil
		result.Outcome = "requirement_drift"
		applied, commitErr := e.Store.CommitActionFailure(ctx, result, run, "brief_invalidated")
		if commitErr != nil {
			return nil, false, commitErr
		}
		return run, applied, nil
	}
	if err != nil {
		return nil, false, err
	}
	result.Summary = RedactText(result.Summary)
	result.Evidence = normalized
	state := e.Workflow.States[string(run.State)]
	target, ok := state.On[result.Outcome]
	if !ok {
		return nil, false, fmt.Errorf("outcome %q is not valid for state %q", result.Outcome, run.State)
	}
	if action.Type == "reconcile" && result.Outcome == "complete" && run.ResumeState != "" {
		target = string(run.ResumeState)
		run.ResumeState = ""
	}
	if err := e.applyEvidence(run, action.Type, result.Outcome, evidence); err != nil {
		return nil, false, err
	}
	if action.Type == "address_review" && result.Outcome == "complete" && len(run.PendingReviewThreadIDs) > 0 {
		target = string(domain.StateAnalyzingReview)
	}
	run.RetryAttempt = 0
	run.State = domain.State(target)
	run.WaitKind = domain.WaitNone
	run.WaitReason = ""
	run.NextWakeAt = nil
	if run.State == domain.StateAwaitingBriefApproval || run.State == domain.StateAwaitingReviewApproval {
		run.WaitKind = domain.WaitHuman
		run.WaitReason = "artifact approval required"
	}
	if run.State == domain.StateBlocked {
		run.WaitKind = domain.WaitHuman
		run.WaitReason = "user input required"
	}
	if run.State == domain.StateAwaitingReview && result.Outcome == "no_change" {
		run.PollAttempt++
		due := e.nextReviewWake(run.PollAttempt)
		run.NextWakeAt = &due
		run.WaitKind = domain.WaitExternal
		run.WaitReason = "waiting for change-request activity"
	} else if run.State == domain.StateAwaitingReview {
		run.PollAttempt = 0
		due := e.nextReviewWake(0)
		run.NextWakeAt = &due
		run.WaitKind = domain.WaitExternal
		run.WaitReason = "waiting for change-request activity"
	}
	applied, err := e.Store.CommitActionResult(ctx, result, run, "action_completed")
	if err != nil {
		return nil, false, err
	}
	return run, applied, nil
}

func (e *Engine) Fail(ctx context.Context, result domain.ActionResult, reason, class string) (*domain.Run, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, errors.New("failure reason is required")
	}
	reason = RedactText(reason)
	if result.ProtocolVersion != domain.ProtocolVersion {
		return nil, fmt.Errorf("protocol mismatch: got %q", result.ProtocolVersion)
	}
	action, err := e.Store.PendingAction(ctx, result.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return e.Store.GetRun(ctx, result.RunID)
		}
		return nil, err
	}
	if action.ActionID != result.ActionID || action.Nonce != result.Nonce {
		return nil, store.ErrConflict
	}
	run, err := e.Store.GetRun(ctx, result.RunID)
	if err != nil {
		return nil, err
	}
	result.Evidence = nil
	result.Summary = reason
	state := e.Workflow.States[string(run.State)]
	policy, hasPolicy := e.Workflow.RetryPolicies[state.RetryPolicy]
	retryable := class == "transient" || class == "rate_limited"
	run.RetryAttempt++
	if retryable && hasPolicy && run.RetryAttempt < policy.MaxAttempts {
		initial, parseErr := time.ParseDuration(policy.Initial)
		if parseErr != nil {
			return nil, parseErr
		}
		maximum, parseErr := time.ParseDuration(policy.Maximum)
		if parseErr != nil {
			return nil, parseErr
		}
		delay := initial
		for n := 1; n < run.RetryAttempt; n++ {
			delay *= 2
			if delay >= maximum {
				delay = maximum
				break
			}
		}
		due := e.Now().Add(delay)
		run.NextWakeAt = &due
		run.WaitKind = domain.WaitExternal
		run.WaitReason = fmt.Sprintf("retry %d/%d after %s: %s", run.RetryAttempt, policy.MaxAttempts, delay, reason)
		if policy.Class == "write_reconcile_first" {
			run.ResumeState = run.State
			run.State = domain.StateReconciling
		}
		result.Outcome = class
		applied, commitErr := e.Store.CommitActionFailure(ctx, result, run, "action_retry_scheduled")
		if commitErr != nil {
			return nil, commitErr
		}
		if !applied {
			return run, nil
		}
		return run, nil
	}
	run.ResumeState = run.State
	run.State = domain.StateBlocked
	run.WaitKind = domain.WaitError
	run.WaitReason = reason
	run.NextWakeAt = nil
	result.Outcome = class
	_, err = e.Store.CommitActionFailure(ctx, result, run, "action_blocked")
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (e *Engine) Approve(ctx context.Context, runID, hash, actor string) (*domain.Run, error) {
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	state := e.Workflow.States[string(run.State)]
	if state.Gate == "" {
		return nil, fmt.Errorf("state %q has no approval gate", run.State)
	}
	var expected, kind string
	switch state.Gate {
	case "approve_brief":
		expected = run.BriefHash
		kind = "brief"
	case "approve_review_plan":
		expected = run.ReviewPlanHash
		kind = "review_plan"
	default:
		return nil, fmt.Errorf("gate %q requires a specialized approval", state.Gate)
	}
	hash = strings.TrimSpace(hash)
	if len(hash) < 8 || expected == "" || !strings.HasPrefix(expected, hash) {
		return nil, fmt.Errorf("approval hash prefix must contain at least 8 matching characters for current %s artifact", kind)
	}
	actor = RedactText(strings.TrimSpace(actor))
	if actor == "" {
		return nil, errors.New("approver identity is required")
	}
	target, ok := state.On["approved"]
	if !ok {
		return nil, errors.New("approval transition missing")
	}
	from := run.State
	run.State = domain.State(target)
	run.WaitKind = domain.WaitNone
	run.WaitReason = ""
	if err := e.Store.CommitApproval(ctx, run, from, kind, expected, actor); err != nil {
		return nil, err
	}
	return run, nil
}

func (e *Engine) Pause(ctx context.Context, runID, reason string) (*domain.Run, error) {
	reason = RedactText(reason)
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.State.Terminal() {
		return nil, ErrTerminal
	}
	run.ResumeState = run.State
	run.State = domain.StatePaused
	run.WaitKind = domain.WaitHuman
	run.WaitReason = reason
	run.NextWakeAt = nil
	if err := e.Store.UpdateRun(ctx, run, "run_paused", "", map[string]string{"reason": reason}); err != nil {
		return nil, err
	}
	return run, nil
}

func (e *Engine) Resume(ctx context.Context, runID string) (*domain.Run, error) {
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.State != domain.StatePaused && run.State != domain.StateBlocked {
		return run, nil
	}
	if run.ResumeState == "" {
		run.ResumeState = domain.StateDiscoveringContext
	}
	run.State = domain.StateReconciling
	run.WaitKind = domain.WaitNone
	run.WaitReason = ""
	if err := e.Store.UpdateRun(ctx, run, "run_resumed", "", nil); err != nil {
		return nil, err
	}
	return run, nil
}

func (e *Engine) Terminal(ctx context.Context, runID string, target domain.State, reason, actor string) (*domain.Run, error) {
	if target != domain.StateCancelled && target != domain.StateFailedPermanent && target != domain.StateSuperseded {
		return nil, errors.New("unsupported explicit terminal state")
	}
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(actor) == "" {
		return nil, errors.New("terminal reason and actor are required")
	}
	reason = RedactText(reason)
	actor = RedactText(actor)
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.State == domain.StateResolved {
		return nil, ErrTerminal
	}
	run.State = target
	run.WaitKind = domain.WaitNone
	run.WaitReason = ""
	run.NextWakeAt = nil
	if err := e.Store.UpdateRun(ctx, run, "run_terminated", "", map[string]string{"reason": reason, "actor": actor}); err != nil {
		return nil, err
	}
	return run, nil
}

func missingCapabilities(required []string, available map[string]bool) []string {
	var out []string
	for _, cap := range required {
		if !available[cap] {
			out = append(out, cap)
		}
	}
	return out
}

func retryClass(def *workflow.Definition, state workflow.State) string {
	if p, ok := def.RetryPolicies[state.RetryPolicy]; ok {
		return p.Class
	}
	return "none"
}

func intentFor(action string) string {
	m := map[string]string{"reconcile": "Re-read relevant requirement, repository, tracker, and change-request state before resuming", "inspect_context": "Read the work item and discover required requirement artifacts", "prepare_workspace": "Create an isolated workspace and change branch", "implement_change": "Implement only the approved execution brief", "verify_change": "Run contract-appropriate verification for the current HEAD", "finalize_change": "Check repository conventions and remove temporary scaffold", "publish_change": "Push the branch, open or reconcile the change request, then update the tracker", "poll_change_request": "Read and normalize new change-request events", "analyze_review": "Analyze the new review batch and produce a response plan", "address_review": "Apply the approved review plan, verify, push, and reply to threads", "resolve_work_item": "Reconcile the observed merge and resolve the tracked work item"}
	return m[action]
}

func evidenceFor(action string) domain.EvidenceRequirement {
	fields := map[string][]string{"reconcile": {"observed_state", "source_revisions"}, "inspect_context": {"work_item", "brief", "brief_hash", "source_revisions"}, "prepare_workspace": {"workspace", "branch", "base_head", "receipt"}, "implement_change": {"head", "summary"}, "verify_change": {"head", "commands", "clean"}, "finalize_change": {"head", "checklist", "clean"}, "publish_change": {"change_request", "tracker_status", "receipts", "source_revisions", "work_item_revision", "head", "clean", "reconciled"}, "poll_change_request": {"change_request_state", "events"}, "analyze_review": {"review_plan", "review_plan_hash", "thread_ids"}, "address_review": {"head", "commands", "push_receipt", "thread_receipts", "source_revisions", "work_item_revision", "reconciled"}, "resolve_work_item": {"merge_receipt", "transition_receipt", "tracker_status", "reconciled"}}
	reconcile := action == "publish_change" || action == "address_review" || action == "resolve_work_item"
	return domain.EvidenceRequirement{Kind: action, RequiredFields: fields[action], Reconcile: reconcile, Schema: EvidenceSchema(action)}
}

func (e *Engine) applyEvidence(run *domain.Run, action, outcome string, value map[string]json.RawMessage) error {
	readString := func(name string) string { var s string; _ = json.Unmarshal(value[name], &s); return s }
	switch action {
	case "inspect_context":
		var workItem domain.WorkItem
		if err := json.Unmarshal(value["work_item"], &workItem); err != nil {
			return err
		}
		run.WorkItem = &workItem
		var brief domain.Brief
		if err := json.Unmarshal(value["brief"], &brief); err != nil {
			return fmt.Errorf("decode brief: %w", err)
		}
		computed, err := CanonicalHash(brief)
		if err != nil {
			return err
		}
		provided := readString("brief_hash")
		if computed != provided {
			return fmt.Errorf("brief_hash mismatch: computed %s", computed)
		}
		run.BriefHash = computed
		if err := json.Unmarshal(value["source_revisions"], &run.SourceRevisions); err != nil {
			return err
		}
	case "prepare_workspace":
		run.Workspace = readString("workspace")
		run.Branch = readString("branch")
		run.Head = readString("base_head")
	case "implement_change":
		run.Head = readString("head")
		run.VerifiedHead = ""
	case "verify_change":
		run.Head = readString("head")
		if outcome == "passed" {
			run.VerifiedHead = run.Head
		} else {
			run.VerifiedHead = ""
		}
	case "finalize_change":
		run.Head = readString("head")
	case "publish_change":
		var cr domain.ChangeRequest
		if err := json.Unmarshal(value["change_request"], &cr); err != nil {
			return err
		}
		run.ChangeRequest = &cr
	case "poll_change_request":
		_, err := e.processReviewEvidence(run, outcome, value)
		if err != nil {
			return err
		}
		if run.ChangeRequest != nil {
			run.ChangeRequest.State = readString("change_request_state")
		}
		if raw, ok := value["last_seen_at"]; ok {
			_ = json.Unmarshal(raw, &run.LastSeenAt)
		}
	case "analyze_review":
		var plan any
		if err := json.Unmarshal(value["review_plan"], &plan); err != nil {
			return fmt.Errorf("decode review plan: %w", err)
		}
		computed, err := CanonicalHash(plan)
		if err != nil {
			return err
		}
		provided := readString("review_plan_hash")
		if computed != provided {
			return fmt.Errorf("review_plan_hash mismatch: computed %s", computed)
		}
		run.ReviewPlanHash = computed
		if err := json.Unmarshal(value["thread_ids"], &run.ReviewPlanThreadIDs); err != nil {
			return err
		}
		run.PendingReviewThreadIDs = nil
	case "reconcile":
		var evidence domain.ReconcileEvidence
		if err := json.Unmarshal(mustMarshalMap(value), &evidence); err != nil {
			return err
		}
		if evidence.Head != "" {
			if evidence.Head != run.Head {
				run.VerifiedHead = ""
			}
			run.Head = evidence.Head
		}
		if evidence.ChangeRequest != nil {
			run.ChangeRequest = evidence.ChangeRequest
		}
	case "address_review":
		run.Head = readString("head")
		run.VerifiedHead = run.Head
		run.ReviewPlanHash = ""
		run.ReviewPlanThreadIDs = nil
	case "resolve_work_item":
		if run.ChangeRequest == nil {
			return errors.New("merge resolution requires a change request")
		}
		run.ChangeRequest.MergeReceipt = value["merge_receipt"]
	}
	return nil
}

func mustMarshalMap(value map[string]json.RawMessage) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func (e *Engine) nextReviewWake(attempt int) time.Time {
	intervals := []time.Duration{10 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	idx := attempt
	if idx >= len(intervals) {
		idx = len(intervals) - 1
	}
	candidate := e.Now().Add(intervals[idx]).In(e.workZone)
	for {
		minute := candidate.Hour()*60 + candidate.Minute()
		if candidate.Weekday() != time.Saturday && candidate.Weekday() != time.Sunday && minute >= e.workStart && minute < e.workEnd {
			return candidate.UTC()
		}
		if candidate.Weekday() != time.Saturday && candidate.Weekday() != time.Sunday && minute < e.workStart {
			return time.Date(candidate.Year(), candidate.Month(), candidate.Day(), e.workStart/60, e.workStart%60, 0, 0, e.workZone).UTC()
		}
		candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day()+1, e.workStart/60, e.workStart%60, 0, 0, e.workZone)
	}
}
