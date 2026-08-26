package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bamanoz/workrun/internal/config"
	"github.com/bamanoz/workrun/internal/domain"
)

const defaultMaxEvidenceBytes = 1 << 20

var sensitiveValues = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:bearer|basic)\s+[a-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`(?i)(?:token|password|secret|api[_-]?key|cookie|set-cookie|session(?:id)?|authorization|signature)\s*[:=]\s*[^\s,;]+`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{10,}|(?:AKIA|ASIA)[A-Z0-9]{16})\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`(?i)[?&](?:x-amz-signature|signature|sig|access_token|token)=[^&#\s]+`),
	regexp.MustCompile(`(?i)://[^/@\s:]+:[^/@\s]+@`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
}
var sha256Value = regexp.MustCompile(`^[0-9a-f]{64}$`)

func decodeStrictJSON(raw []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func newEvidence(action string) (any, error) {
	switch action {
	case "inspect_context":
		return &domain.InspectContextEvidence{}, nil
	case "reconcile":
		return &domain.ReconcileEvidence{}, nil
	case "prepare_workspace":
		return &domain.PrepareWorkspaceEvidence{}, nil
	case "implement_change":
		return &domain.ImplementEvidence{}, nil
	case "verify_change":
		return &domain.VerifyEvidence{}, nil
	case "finalize_change":
		return &domain.FinalizeEvidence{}, nil
	case "publish_change":
		return &domain.PublishEvidence{}, nil
	case "poll_change_request":
		return &domain.PollEvidence{}, nil
	case "analyze_review":
		return &domain.AnalyzeReviewEvidence{}, nil
	case "address_review":
		return &domain.AddressReviewEvidence{}, nil
	case "resolve_work_item":
		return &domain.ResolveWorkItemEvidence{}, nil
	case "schedule_wake":
		return &domain.WakeEvidence{}, nil
	default:
		return nil, fmt.Errorf("%w: unknown action %q", ErrBadEvidence, action)
	}
}

func (e *Engine) validateAndNormalizeEvidence(ctx context.Context, run *domain.Run, action, outcome string, raw json.RawMessage) (json.RawMessage, map[string]json.RawMessage, error) {
	limit := defaultMaxEvidenceBytes
	if e.User != nil && e.User.Trust.MaxContentBytes > 0 {
		limit = e.User.Trust.MaxContentBytes
	}
	if len(raw) == 0 || len(raw) > limit {
		return nil, nil, fmt.Errorf("%w: evidence size %d exceeds limit %d", ErrBadEvidence, len(raw), limit)
	}
	value, err := newEvidence(action)
	if err != nil {
		return nil, nil, err
	}
	if err := decodeStrictJSON(raw, value); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrBadEvidence, err)
	}
	if err := e.validateTypedEvidence(ctx, run, outcome, value); err != nil {
		if errors.Is(err, ErrRequirementDrift) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: %v", ErrBadEvidence, err)
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	normalized = redactJSON(normalized)
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(normalized, &fields); err != nil {
		return nil, nil, err
	}
	return normalized, fields, nil
}

func (e *Engine) validateTypedEvidence(ctx context.Context, run *domain.Run, outcome string, value any) error {
	switch v := value.(type) {
	case *domain.InspectContextEvidence:
		return e.validateBrief(ctx, run, outcome, v)
	case *domain.ReconcileEvidence:
		if strings.TrimSpace(v.ObservedState) == "" {
			return errors.New("observed_state is required")
		}
		if err := validateSourceRevisions(v.SourceRevisions); err != nil {
			return err
		}
		if len(run.SourceRevisions) > 0 && !sameRevisions(run.SourceRevisions, v.SourceRevisions) {
			return fmt.Errorf("%w: reconciliation observed source revision drift", ErrRequirementDrift)
		}
		if run.WorkItem != nil {
			if v.WorkItemRevision == "" {
				return errors.New("work_item_revision is required when reconciling an approved brief")
			}
			if v.WorkItemRevision != run.WorkItem.Revision {
				return fmt.Errorf("%w: reconciliation observed work item revision drift", ErrRequirementDrift)
			}
		}
		if v.ChangeRequest != nil {
			if err := e.validateChangeRequest(*v.ChangeRequest); err != nil {
				return err
			}
			if run.ChangeRequest != nil && (v.ChangeRequest.Provider != run.ChangeRequest.Provider || v.ChangeRequest.ExternalID != run.ChangeRequest.ExternalID) {
				return errors.New("reconciliation cannot replace the persisted change request")
			}
			if v.Head != "" && v.ChangeRequest.Head != v.Head {
				return errors.New("reconciled change request head does not match observed head")
			}
		}
	case *domain.PrepareWorkspaceEvidence:
		if v.Workspace == "" || v.Branch == "" || v.BaseHead == "" {
			return errors.New("workspace, branch, and base_head are required")
		}
		if err := e.validateReceipt(run, v.Receipt, "workspace.create"); err != nil {
			return err
		}
		if v.Receipt.Head != v.BaseHead || !receiptSubject(v.Receipt, "local", run.Repository) {
			return errors.New("workspace receipt head must match base_head")
		}
	case *domain.ImplementEvidence:
		if v.Head == "" || strings.TrimSpace(v.Summary) == "" {
			return errors.New("head and summary are required")
		}
	case *domain.VerifyEvidence:
		if v.Head == "" || len(v.Commands) == 0 {
			return errors.New("head and commands are required")
		}
		if v.Head != run.Head {
			return errors.New("verification head does not match current run head")
		}
		if outcome == "passed" {
			if !v.Clean {
				return errors.New("passed verification requires clean=true")
			}
			return validateCommands(v.Commands, v.Head, true)
		}
		if outcome == "failed_fixable" {
			return validateCommands(v.Commands, v.Head, false)
		}
		return errors.New("unsupported verification outcome")
	case *domain.FinalizeEvidence:
		if v.Head == "" || len(v.Checklist) == 0 || string(v.Checklist) == "null" {
			return errors.New("head and checklist are required")
		}
		if v.Head != run.VerifiedHead {
			return errors.New("finalization head does not match passed verification head")
		}
		if outcome == "needs_changes" {
			return nil
		}
		if outcome != "complete" || !v.Clean || v.CommitReceipt == nil {
			return errors.New("completed finalization requires clean=true and commit_receipt")
		}
		if err := e.validateReceipt(run, *v.CommitReceipt, "vcs.commit"); err != nil {
			return err
		}
		if v.CommitReceipt.Head != v.Head || !receiptSubject(*v.CommitReceipt, "local", run.Repository) {
			return errors.New("commit receipt must match final head and repository")
		}
	case *domain.PublishEvidence:
		if !v.Clean || !v.Reconciled || v.Head == "" || v.TrackerStatus != "in_review" {
			return errors.New("clean=true, reconciled=true, head, and tracker_status=in_review are required")
		}
		if v.Head != run.Head || v.Head != run.VerifiedHead {
			return errors.New("publication head lacks fresh successful verification")
		}
		if v.ChangeRequest.Head != v.Head {
			return errors.New("change request head does not match verified head")
		}
		if v.ChangeRequest.State != "open" || v.Receipts.Review.State != "open" {
			return errors.New("change request and receipt must confirm open state")
		}
		if err := e.validateChangeRequest(v.ChangeRequest); err != nil {
			return err
		}
		for operation, receipt := range map[string]domain.Receipt{"vcs.push": v.Receipts.Push, "change_request.open": v.Receipts.Review, "tracker.transition": v.Receipts.Transition, "tracker.comment": v.Receipts.Comment} {
			if err := e.validateReceipt(run, receipt, operation); err != nil {
				return err
			}
		}
		if v.Receipts.Push.Head != v.Head || v.Receipts.Review.Head != v.Head {
			return errors.New("push and change-request receipts must match published head")
		}
		if !receiptSubject(v.Receipts.Push, "local", run.Repository) || !receiptSubject(v.Receipts.Review, v.ChangeRequest.Provider, v.ChangeRequest.ExternalID) || !receiptSubject(v.Receipts.Transition, run.WorkItemProvider, run.WorkItemID) || !receiptSubject(v.Receipts.Comment, run.WorkItemProvider, run.WorkItemID) {
			return errors.New("publication receipts do not match repository, change request, and work item")
		}
		if v.Receipts.Transition.State != "in_review" {
			return errors.New("tracker transition receipt must confirm in_review state")
		}
		if err := validateSourceRevisions(v.SourceRevisions); err != nil {
			return err
		}
		if !sameRevisions(run.SourceRevisions, v.SourceRevisions) {
			return fmt.Errorf("%w: publication source revisions differ from approved brief", ErrRequirementDrift)
		}
		if run.WorkItem == nil || v.WorkItemRevision != run.WorkItem.Revision {
			return fmt.Errorf("%w: publication work item revision differs from approved brief", ErrRequirementDrift)
		}
	case *domain.PollEvidence:
		if v.ChangeRequestState == "" || v.Cursor == "" && v.LastSeenAt == "" {
			return errors.New("change_request_state and cursor or last_seen_at are required")
		}
		for i := range v.Events {
			if err := validateReviewEvent(&v.Events[i]); err != nil {
				return err
			}
		}
	case *domain.AnalyzeReviewEvidence:
		if len(v.ReviewPlan) == 0 || v.ReviewPlanHash == "" || len(v.ThreadIDs) == 0 {
			return errors.New("review_plan, review_plan_hash, and thread_ids are required")
		}
		var plan any
		if err := json.Unmarshal(v.ReviewPlan, &plan); err != nil {
			return err
		}
		if !sameStrings(v.ThreadIDs, run.PendingReviewThreadIDs) {
			return errors.New("review plan thread_ids must exactly match pending review threads")
		}
		hash, _ := CanonicalHash(plan)
		if hash != v.ReviewPlanHash {
			return errors.New("review_plan_hash mismatch")
		}
	case *domain.AddressReviewEvidence:
		if !v.Reconciled || v.Head == "" || len(v.Commands) == 0 || len(v.ThreadReceipts) == 0 {
			return errors.New("reconciled=true, head, commands, and thread receipts are required")
		}
		if err := validateCommands(v.Commands, v.Head, true); err != nil {
			return err
		}
		if err := validateSourceRevisions(v.SourceRevisions); err != nil {
			return err
		}
		if !sameRevisions(run.SourceRevisions, v.SourceRevisions) {
			return fmt.Errorf("%w: review push source revisions differ from approved brief", ErrRequirementDrift)
		}
		if run.WorkItem == nil || v.WorkItemRevision != run.WorkItem.Revision {
			return fmt.Errorf("%w: review work item revision differs from approved brief", ErrRequirementDrift)
		}
		if run.ChangeRequest == nil {
			return errors.New("review response requires a change request")
		}
		if err := e.validateReceipt(run, v.PushReceipt, "vcs.push"); err != nil {
			return err
		}
		if v.PushReceipt.Head != v.Head || !receiptSubject(v.PushReceipt, "local", run.Repository) {
			return errors.New("review push receipt must match addressed head and repository")
		}
		covered := make([]string, 0, len(v.ThreadReceipts))
		for _, receipt := range v.ThreadReceipts {
			if receipt.ThreadID == "" || receipt.ReplyID == "" || receipt.Commit != v.Head || receipt.SubjectProvider != run.ChangeRequest.Provider || receipt.SubjectID != run.ChangeRequest.ExternalID || (receipt.Outcome != "fixed" && receipt.Outcome != "rejected") || receipt.ObservedAt == "" {
				return errors.New("invalid thread receipt")
			}
			observed, err := time.Parse(time.RFC3339, receipt.ObservedAt)
			if err != nil || e.Now != nil && observed.After(e.Now().Add(5*time.Minute)) || !run.UpdatedAt.IsZero() && observed.Before(run.UpdatedAt.Add(-5*time.Minute)) {
				return errors.New("invalid or stale thread receipt timestamp")
			}
			covered = append(covered, receipt.ThreadID)
		}
		if !sameStrings(covered, run.ReviewPlanThreadIDs) {
			return errors.New("thread receipts must exactly cover approved review threads")
		}
	case *domain.ResolveWorkItemEvidence:
		if !v.Reconciled || v.TrackerStatus != "resolved" {
			return errors.New("reconciled=true and tracker_status=resolved are required")
		}
		if err := e.validateReceipt(run, v.MergeReceipt, "change_request.merge"); err != nil {
			return err
		}
		if err := e.validateReceipt(run, v.TransitionReceipt, "tracker.transition"); err != nil {
			return err
		}
		if run.ChangeRequest == nil || v.MergeReceipt.Head == "" || v.MergeReceipt.Head != run.Head {
			return errors.New("merge receipt must confirm current run head")
		}
		if !receiptSubject(v.MergeReceipt, run.ChangeRequest.Provider, run.ChangeRequest.ExternalID) || !receiptSubject(v.TransitionReceipt, run.WorkItemProvider, run.WorkItemID) {
			return errors.New("resolution receipts do not match change request and work item")
		}
		if v.MergeReceipt.State != "merged" || v.TransitionReceipt.State != "resolved" {
			return errors.New("resolution receipts must confirm merged and resolved states")
		}
	case *domain.WakeEvidence:
		if !v.Scheduled || v.Reason == "" {
			return errors.New("scheduled=true and reason are required")
		}
		due, err := time.Parse(time.RFC3339, v.DueAt)
		if err != nil || run.NextWakeAt == nil || !due.Equal(*run.NextWakeAt) {
			return errors.New("due_at must match the persisted RFC3339 schedule")
		}
	}
	return nil
}

func (e *Engine) validateBrief(ctx context.Context, run *domain.Run, outcome string, evidence *domain.InspectContextEvidence) error {
	item := evidence.WorkItem
	if item.Provider == "" || item.ExternalID == "" || item.Key == "" || item.Status == "" || item.Revision == "" {
		return errors.New("normalized work item requires provider, external_id, key, status, and revision")
	}
	if item.Provider != run.WorkItemProvider || item.ExternalID != run.WorkItemID || item.Key != run.WorkItemKey {
		return errors.New("normalized work item identity does not match run")
	}
	if item.Provider != "workrun-evolution" && e.User != nil && !e.User.AllowsProvider(item.Provider) {
		return errors.New("work item provider is not trusted")
	}
	if item.URL != "" && !e.allowsURL(item.URL) {
		return errors.New("work item URL is not trusted")
	}
	for _, relationship := range item.Relationships {
		if relationship.Type == "" || relationship.ID == "" {
			return errors.New("work item relationships require type and id")
		}
		if relationship.URL != "" && !e.allowsURL(relationship.URL) {
			return errors.New("relationship URL is not trusted")
		}
	}
	brief := evidence.Brief
	if strings.TrimSpace(brief.Problem) == "" || len(brief.Scope) == 0 || len(brief.AcceptanceCriteria) == 0 || strings.TrimSpace(brief.TestStrategy) == "" {
		return errors.New("brief requires problem, scope, acceptance criteria, and test strategy")
	}
	incomplete := outcome == "needs_input"
	if !incomplete && len(brief.OpenQuestions) != 0 {
		return errors.New("brief has unresolved open questions")
	}
	if incomplete && len(brief.OpenQuestions) == 0 && len(evidence.MissingRoles) == 0 && len(evidence.Conflicts) == 0 {
		return errors.New("provisional brief must identify a missing role, conflict, or open question")
	}
	raw, err := e.Store.GetManifest(ctx, run.ManifestHash)
	if err != nil {
		return fmt.Errorf("load repository manifest: %w", err)
	}
	manifest, err := config.ParseRepository(raw)
	if err != nil {
		return err
	}
	roles := map[string]bool{}
	sources := map[string]domain.RequirementSource{}
	for _, source := range brief.RequirementSources {
		if source.Role == "" || source.ExternalID == "" || source.Revision == "" {
			return errors.New("requirement sources require role, external_id, and revision")
		}
		roles[source.Role] = true
		sources[source.Role+"\x00"+source.ExternalID] = source
		if source.URL != "" && !e.allowsURL(source.URL) {
			return fmt.Errorf("requirement URL is not trusted: %s", source.URL)
		}
		if source.Provider != "" && e.User != nil && !e.User.AllowsProvider(source.Provider) {
			return fmt.Errorf("requirement provider is not trusted: %s", source.Provider)
		}
	}
	missing := map[string]bool{}
	for _, role := range evidence.MissingRoles {
		if role == "" || missing[role] {
			return errors.New("missing_roles must be unique and non-empty")
		}
		missing[role] = true
	}
	for _, role := range manifest.Requirements.RequiredRoles {
		if !roles[role] && (!incomplete || !missing[role]) {
			return fmt.Errorf("required requirement role %q is missing", role)
		}
		if roles[role] && missing[role] {
			return fmt.Errorf("role %q is both present and missing", role)
		}
	}
	if err := validateSourceRevisions(evidence.SourceRevisions); err != nil {
		return err
	}
	if len(evidence.SourceRevisions) != len(sources) {
		return errors.New("source_revisions must exactly cover requirement sources")
	}
	for _, revision := range evidence.SourceRevisions {
		source, ok := sources[revision.Role+"\x00"+revision.ExternalID]
		if !ok || source.Revision != revision.Revision {
			return errors.New("source revision does not match brief source")
		}
	}
	hash, _ := CanonicalHash(brief)
	if hash != evidence.BriefHash {
		return errors.New("brief_hash mismatch")
	}
	return nil
}

func validateSourceRevisions(revisions []domain.SourceRevision) error {
	seen := map[string]bool{}
	for _, revision := range revisions {
		key := revision.Role + "\x00" + revision.ExternalID
		if revision.Role == "" || revision.ExternalID == "" || revision.Revision == "" || seen[key] {
			return errors.New("source revisions require unique role, external_id, and revision")
		}
		seen[key] = true
	}
	return nil
}

func sameRevisions(left, right []domain.SourceRevision) bool {
	if len(left) != len(right) {
		return false
	}
	values := map[string]string{}
	for _, item := range left {
		values[item.Role+"\x00"+item.ExternalID] = item.Revision
	}
	for _, item := range right {
		if values[item.Role+"\x00"+item.ExternalID] != item.Revision {
			return false
		}
	}
	return true
}

func validateCommands(commands []domain.CommandEvidence, head string, requireSuccess bool) error {
	failed := false
	for _, command := range commands {
		if len(command.Argv) == 0 || command.CWD == "" || command.Head != head || !sha256Value.MatchString(command.OutputHash) {
			return errors.New("each command requires argv, cwd, SHA-256 output_hash, and matching head")
		}
		if command.ExitCode != 0 {
			failed = true
		}
	}
	if requireSuccess && failed {
		return errors.New("successful verification requires exit_code=0")
	}
	if !requireSuccess && !failed {
		return errors.New("failed_fixable verification requires a non-zero exit code")
	}
	return nil
}

func (e *Engine) processReviewEvidence(run *domain.Run, outcome string, value map[string]json.RawMessage) ([]json.RawMessage, error) {
	var poll domain.PollEvidence
	if err := json.Unmarshal(mustMarshalMap(value), &poll); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, id := range run.SeenReviewEventIDs {
		seen[id] = true
	}
	var rawEvents []json.RawMessage
	var actionable []string
	terminal := ""
	for _, event := range poll.Events {
		key := event.ID
		if key == "" {
			key = event.Fingerprint
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		run.SeenReviewEventIDs = append(run.SeenReviewEventIDs, key)
		raw, _ := json.Marshal(event)
		rawEvents = append(rawEvents, raw)
		if event.Type == "actionable_comment" {
			actionable = append(actionable, event.ThreadID)
		}
		if event.Type == "merged" {
			terminal = "merged"
		}
		if event.Type == "closed" || event.Type == "declined" {
			terminal = "closed"
		}
	}
	if terminal != "" {
		if outcome != terminal {
			return nil, fmt.Errorf("outcome %q does not match observed %s event", outcome, terminal)
		}
	} else if len(actionable) > 0 {
		if run.ReviewPlanHash == "" {
			if outcome != "feedback" {
				return nil, errors.New("new actionable feedback requires feedback outcome")
			}
			run.PendingReviewThreadIDs = appendUnique(run.PendingReviewThreadIDs, actionable...)
		} else {
			switch poll.Overlap {
			case "overlap":
				if outcome != "feedback_overlap" {
					return nil, errors.New("overlapping feedback requires feedback_overlap outcome")
				}
				run.PendingReviewThreadIDs = appendUnique(run.PendingReviewThreadIDs, run.ReviewPlanThreadIDs...)
				run.PendingReviewThreadIDs = appendUnique(run.PendingReviewThreadIDs, actionable...)
				run.ReviewPlanHash = ""
				run.ReviewPlanThreadIDs = nil
			case "independent":
				if outcome != "feedback_independent" {
					return nil, errors.New("independent feedback requires feedback_independent outcome")
				}
				run.PendingReviewThreadIDs = appendUnique(run.PendingReviewThreadIDs, actionable...)
			default:
				return nil, errors.New("feedback during approved review work requires overlap classification")
			}
		}
	} else if outcome != "no_change" {
		return nil, fmt.Errorf("outcome %q has no matching new review event", outcome)
	}
	return rawEvents, nil
}

func appendUnique(existing []string, values ...string) []string {
	seen := map[string]bool{}
	for _, value := range existing {
		seen[value] = true
	}
	for _, value := range values {
		if !seen[value] {
			existing = append(existing, value)
			seen[value] = true
		}
	}
	return existing
}
func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := map[string]bool{}
	for _, value := range left {
		if seen[value] {
			return false
		}
		seen[value] = true
	}
	for _, value := range right {
		if !seen[value] {
			return false
		}
	}
	return true
}

func validateReviewEvent(event *domain.ReviewEvent) error {
	if event.Type == "" || event.ID == "" && event.Fingerprint == "" {
		return errors.New("review event requires type and id or fingerprint")
	}
	switch event.Type {
	case "actionable_comment":
		if event.ThreadID == "" {
			return errors.New("actionable review event requires thread_id")
		}
		return nil
	case "approval", "build_status", "merged", "closed", "declined":
		return nil
	default:
		return fmt.Errorf("unknown review event type %q", event.Type)
	}
}

func (e *Engine) validateReceipt(run *domain.Run, receipt domain.Receipt, operation string) error {
	if receipt.Provider == "" || receipt.ExternalID == "" || receipt.Operation != operation || receipt.SubjectProvider == "" || receipt.SubjectID == "" || receipt.ObservedAt == "" {
		return fmt.Errorf("invalid %s receipt", operation)
	}
	observed, err := time.Parse(time.RFC3339, receipt.ObservedAt)
	if err != nil || e.Now != nil && observed.After(e.Now().Add(5*time.Minute)) || !run.UpdatedAt.IsZero() && observed.Before(run.UpdatedAt.Add(-5*time.Minute)) {
		return fmt.Errorf("invalid or stale %s receipt timestamp", operation)
	}
	if e.User != nil && !e.User.AllowsProvider(receipt.Provider) {
		return fmt.Errorf("receipt provider is not trusted: %s", receipt.Provider)
	}
	if receipt.URL != "" && !e.allowsURL(receipt.URL) {
		return fmt.Errorf("receipt URL is not trusted: %s", receipt.URL)
	}
	return nil
}

func receiptSubject(receipt domain.Receipt, provider, id string) bool {
	return receipt.SubjectProvider == provider && receipt.SubjectID == id
}

func (e *Engine) validateChangeRequest(cr domain.ChangeRequest) error {
	if cr.Provider == "" || cr.ExternalID == "" || cr.URL == "" || cr.State == "" || cr.Head == "" || cr.Base == "" {
		return errors.New("change request is incomplete")
	}
	if e.User != nil && !e.User.AllowsProvider(cr.Provider) {
		return fmt.Errorf("change request provider is not trusted: %s", cr.Provider)
	}
	if !e.allowsURL(cr.URL) {
		return errors.New("change request URL is not trusted")
	}
	return nil
}

func (e *Engine) allowsURL(raw string) bool {
	if e.User != nil {
		return e.User.AllowsURL(raw)
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.User == nil
}

func RedactText(value string) string { return redactString(value) }

func RedactJSON(raw []byte) []byte { return redactJSON(raw) }

func HistoryEventData(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var event map[string]any
	if json.Unmarshal(raw, &event) != nil {
		return nil
	}
	if evidence, ok := event["evidence"].(map[string]any); ok {
		receipts := map[string]any{}
		for _, key := range []string{"receipt", "receipts", "push_receipt", "thread_receipts", "merge_receipt", "transition_receipt"} {
			if value, exists := evidence[key]; exists {
				receipts[key] = value
			}
		}
		if len(receipts) == 0 {
			delete(event, "evidence")
		} else {
			event["evidence"] = receipts
		}
	}
	normalized, _ := json.Marshal(event)
	return redactJSON(normalized)
}

func redactJSON(raw []byte) []byte {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	out, err := json.Marshal(redactValue(value))
	if err != nil {
		return raw
	}
	return out
}

func redactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			lower := strings.ToLower(key)
			if sensitiveKey(lower) {
				v[key] = "[REDACTED]"
			} else {
				v[key] = redactValue(item)
			}
		}
	case []any:
		for i := range v {
			v[i] = redactValue(v[i])
		}
	case string:
		return redactString(v)
	}
	return value
}
func redactString(value string) string {
	for _, pattern := range sensitiveValues {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	return value
}

func sensitiveKey(lower string) bool {
	for _, part := range []string{"token", "password", "secret", "authorization", "api_key", "api-key", "credential", "cookie", "session", "private_key", "access_key", "signature"} {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}
