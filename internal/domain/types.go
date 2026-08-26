package domain

import (
	"encoding/json"
	"time"
)

const (
	ProtocolVersion = "1.0"
	SchemaVersion   = 1
)

type State string

const (
	StateDiscoveringContext      State = "discovering_context"
	StateReconciling             State = "reconciling"
	StateAwaitingBriefApproval   State = "awaiting_brief_approval"
	StateBriefStale              State = "brief_stale"
	StatePreparingWorkspace      State = "preparing_workspace"
	StateImplementing            State = "implementing"
	StateVerifying               State = "verifying"
	StateFinalizing              State = "finalizing"
	StatePublishing              State = "publishing"
	StateAwaitingReview          State = "awaiting_review"
	StateAnalyzingReview         State = "analyzing_review"
	StateAwaitingReviewApproval  State = "awaiting_review_plan_approval"
	StateConfirmingReviewPlan    State = "confirming_review_plan"
	StateAddressingReview        State = "addressing_review"
	StateMergedPendingResolution State = "merged_pending_resolution"
	StateBlocked                 State = "blocked"
	StatePaused                  State = "paused"
	StateResolved                State = "resolved"
	StateCancelled               State = "cancelled"
	StateSuperseded              State = "superseded"
	StateFailedPermanent         State = "failed_permanent"
)

func (s State) Terminal() bool {
	switch s {
	case StateResolved, StateCancelled, StateSuperseded, StateFailedPermanent:
		return true
	default:
		return false
	}
}

type WaitKind string

const (
	WaitNone       WaitKind = ""
	WaitHuman      WaitKind = "human"
	WaitExternal   WaitKind = "external"
	WaitCapability WaitKind = "capability"
	WaitError      WaitKind = "error"
)

type WorkItem struct {
	Provider      string            `json:"provider"`
	ExternalID    string            `json:"external_id"`
	Key           string            `json:"key"`
	URL           string            `json:"url,omitempty"`
	Title         string            `json:"title,omitempty"`
	Status        string            `json:"status,omitempty"`
	Relationships []Relationship    `json:"relationships,omitempty"`
	Assignee      string            `json:"assignee,omitempty"`
	Reporter      string            `json:"reporter,omitempty"`
	Labels        []string          `json:"labels,omitempty"`
	Revision      string            `json:"revision,omitempty"`
	Fields        map[string]string `json:"fields,omitempty"`
}

type Relationship struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	URL  string `json:"url,omitempty"`
}

type RequirementSource struct {
	Role         string `json:"role"`
	Provider     string `json:"provider,omitempty"`
	ExternalID   string `json:"external_id"`
	URL          string `json:"url,omitempty"`
	Revision     string `json:"revision_or_hash"`
	Title        string `json:"title,omitempty"`
	Summary      string `json:"summary,omitempty"`
	UserSupplied bool   `json:"user_supplied,omitempty"`
}

type Brief struct {
	Problem            string              `json:"problem"`
	Scope              []string            `json:"scope"`
	NonGoals           []string            `json:"non_goals"`
	AcceptanceCriteria []string            `json:"acceptance_criteria"`
	Constraints        []string            `json:"constraints"`
	RequirementSources []RequirementSource `json:"requirement_sources"`
	TestStrategy       string              `json:"test_strategy"`
	OpenQuestions      []string            `json:"open_questions"`
	ExplicitOverrides  []string            `json:"explicit_overrides"`
}

type Approval struct {
	Kind         string    `json:"kind"`
	ArtifactHash string    `json:"artifact_hash"`
	ApprovedAt   time.Time `json:"approved_at"`
	ApprovedBy   string    `json:"approved_by"`
}

type ChangeRequest struct {
	Provider     string          `json:"provider"`
	ExternalID   string          `json:"external_id"`
	URL          string          `json:"url"`
	State        string          `json:"state"`
	Head         string          `json:"head"`
	Base         string          `json:"base"`
	Cursor       string          `json:"cursor,omitempty"`
	MergeReceipt json.RawMessage `json:"merge_receipt,omitempty"`
}

type Run struct {
	ID                     string           `json:"id"`
	GroupID                string           `json:"group_id"`
	WorkItemProvider       string           `json:"work_item_provider"`
	WorkItemID             string           `json:"work_item_id"`
	WorkItemKey            string           `json:"work_item_key"`
	Repository             string           `json:"repository"`
	WorkItem               *WorkItem        `json:"work_item,omitempty"`
	State                  State            `json:"state"`
	ResumeState            State            `json:"resume_state,omitempty"`
	WaitKind               WaitKind         `json:"wait_kind,omitempty"`
	WaitReason             string           `json:"wait_reason,omitempty"`
	WorkflowName           string           `json:"workflow_name"`
	WorkflowHash           string           `json:"workflow_hash"`
	ProtocolVersion        string           `json:"protocol_version"`
	ManifestHash           string           `json:"manifest_hash,omitempty"`
	Workspace              string           `json:"workspace,omitempty"`
	Branch                 string           `json:"branch,omitempty"`
	Head                   string           `json:"head,omitempty"`
	VerifiedHead           string           `json:"verified_head,omitempty"`
	BriefHash              string           `json:"brief_hash,omitempty"`
	ReviewPlanHash         string           `json:"review_plan_hash,omitempty"`
	SourceRevisions        []SourceRevision `json:"source_revisions,omitempty"`
	ReviewPlanThreadIDs    []string         `json:"review_plan_thread_ids,omitempty"`
	LastSeenAt             string           `json:"last_seen_at,omitempty"`
	SeenReviewEventIDs     []string         `json:"seen_review_event_ids,omitempty"`
	PendingReviewThreadIDs []string         `json:"pending_review_thread_ids,omitempty"`
	ChangeRequest          *ChangeRequest   `json:"change_request,omitempty"`
	NextWakeAt             *time.Time       `json:"next_wake_at,omitempty"`
	PollAttempt            int              `json:"poll_attempt"`
	RetryAttempt           int              `json:"retry_attempt"`
	Version                int64            `json:"version"`
	Supersedes             string           `json:"supersedes,omitempty"`
	CreatedAt              time.Time        `json:"created_at"`
	UpdatedAt              time.Time        `json:"updated_at"`
}

type Event struct {
	ID        int64           `json:"id"`
	RunID     string          `json:"run_id"`
	Sequence  int64           `json:"sequence"`
	Type      string          `json:"type"`
	FromState State           `json:"from_state,omitempty"`
	ToState   State           `json:"to_state,omitempty"`
	ActionID  string          `json:"action_id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type CapabilityInventory struct {
	ProtocolVersion string            `json:"protocol_version"`
	Host            string            `json:"host"`
	Capabilities    map[string]bool   `json:"capabilities"`
	ToolSchemas     map[string]string `json:"tool_schema_hashes,omitempty"`
}

type Lease struct {
	RunID     string    `json:"run_id"`
	Token     string    `json:"token"`
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ActionEnvelope struct {
	ProtocolVersion  string              `json:"protocol_version"`
	SchemaVersion    int                 `json:"schema_version"`
	RunID            string              `json:"run_id"`
	ActionID         string              `json:"action_id"`
	Nonce            string              `json:"nonce"`
	Lease            Lease               `json:"lease"`
	Type             string              `json:"type"`
	AllowedOutcomes  []string            `json:"allowed_outcomes,omitempty"`
	Intent           string              `json:"intent"`
	Inputs           json.RawMessage     `json:"inputs"`
	RequiredEvidence EvidenceRequirement `json:"required_evidence"`
	RetryClass       string              `json:"retry_class"`
	Deadline         time.Time           `json:"deadline"`
}

type EvidenceRequirement struct {
	Kind           string          `json:"kind"`
	RequiredFields []string        `json:"required_fields"`
	Reconcile      bool            `json:"reconcile"`
	Schema         json.RawMessage `json:"schema,omitempty"`
}

type ActionResult struct {
	ProtocolVersion string          `json:"protocol_version"`
	RunID           string          `json:"run_id"`
	ActionID        string          `json:"action_id"`
	Nonce           string          `json:"nonce"`
	LeaseToken      string          `json:"lease_token"`
	Outcome         string          `json:"outcome"`
	Evidence        json.RawMessage `json:"evidence"`
	Summary         string          `json:"summary,omitempty"`
}

type Proposal struct {
	ID           string          `json:"id"`
	RunID        string          `json:"run_id,omitempty"`
	Scope        string          `json:"scope"`
	Reason       string          `json:"reason"`
	ArtifactHash string          `json:"artifact_hash"`
	Diff         json.RawMessage `json:"diff"`
	Status       string          `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
	ApprovedAt   *time.Time      `json:"approved_at,omitempty"`
}
