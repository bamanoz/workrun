package domain

import "encoding/json"

type SourceRevision struct {
	Role       string `json:"role"`
	ExternalID string `json:"external_id"`
	Revision   string `json:"revision_or_hash"`
}

type CommandEvidence struct {
	Argv       []string `json:"argv"`
	CWD        string   `json:"cwd"`
	ExitCode   int      `json:"exit_code"`
	OutputHash string   `json:"output_hash"`
	Head       string   `json:"head"`
}

type Receipt struct {
	Provider        string `json:"provider"`
	ExternalID      string `json:"external_id"`
	Operation       string `json:"operation"`
	SubjectProvider string `json:"subject_provider"`
	SubjectID       string `json:"subject_id"`
	State           string `json:"state,omitempty"`
	URL             string `json:"url,omitempty"`
	Head            string `json:"head,omitempty"`
	ObservedAt      string `json:"observed_at"`
}

type ThreadReceipt struct {
	ThreadID        string `json:"thread_id"`
	SubjectProvider string `json:"subject_provider"`
	SubjectID       string `json:"subject_id"`
	ReplyID         string `json:"reply_id"`
	Commit          string `json:"commit"`
	Outcome         string `json:"outcome"`
	Resolved        bool   `json:"resolved"`
	ObservedAt      string `json:"observed_at"`
}

type ReviewEvent struct {
	ID          string `json:"id,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Type        string `json:"type"`
	ThreadID    string `json:"thread_id,omitempty"`
	Author      string `json:"author,omitempty"`
	Body        string `json:"body,omitempty"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Head        string `json:"head,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

type InspectContextEvidence struct {
	WorkItem        WorkItem         `json:"work_item"`
	Brief           Brief            `json:"brief"`
	BriefHash       string           `json:"brief_hash"`
	SourceRevisions []SourceRevision `json:"source_revisions"`
	MissingRoles    []string         `json:"missing_roles,omitempty"`
	Conflicts       []string         `json:"conflicts,omitempty"`
}

type ReconcileEvidence struct {
	ObservedState    string           `json:"observed_state"`
	SourceRevisions  []SourceRevision `json:"source_revisions"`
	WorkItemRevision string           `json:"work_item_revision,omitempty"`
	Head             string           `json:"head,omitempty"`
	TrackerStatus    string           `json:"tracker_status,omitempty"`
	ChangeRequest    *ChangeRequest   `json:"change_request,omitempty"`
	Drift            []string         `json:"drift,omitempty"`
}

type PrepareWorkspaceEvidence struct {
	Workspace string  `json:"workspace"`
	Branch    string  `json:"branch"`
	BaseHead  string  `json:"base_head"`
	Receipt   Receipt `json:"receipt"`
}

type ImplementEvidence struct {
	Head    string `json:"head"`
	Summary string `json:"summary"`
}

type VerifyEvidence struct {
	Head     string            `json:"head"`
	Commands []CommandEvidence `json:"commands"`
	Clean    bool              `json:"clean"`
}

type FinalizeEvidence struct {
	Head          string          `json:"head"`
	Checklist     json.RawMessage `json:"checklist"`
	Clean         bool            `json:"clean"`
	CommitReceipt *Receipt        `json:"commit_receipt,omitempty"`
}

type PublishReceipts struct {
	Push       Receipt `json:"push"`
	Review     Receipt `json:"change_request"`
	Transition Receipt `json:"tracker_transition"`
	Comment    Receipt `json:"tracker_comment"`
}

type PublishEvidence struct {
	ChangeRequest    ChangeRequest    `json:"change_request"`
	TrackerStatus    string           `json:"tracker_status"`
	Receipts         PublishReceipts  `json:"receipts"`
	SourceRevisions  []SourceRevision `json:"source_revisions"`
	WorkItemRevision string           `json:"work_item_revision"`
	Head             string           `json:"head"`
	Clean            bool             `json:"clean"`
	Reconciled       bool             `json:"reconciled"`
}

type PollEvidence struct {
	ChangeRequestState string        `json:"change_request_state"`
	Events             []ReviewEvent `json:"events"`
	Overlap            string        `json:"overlap,omitempty"`
	Cursor             string        `json:"cursor,omitempty"`
	LastSeenAt         string        `json:"last_seen_at,omitempty"`
}

type AnalyzeReviewEvidence struct {
	ReviewPlan     json.RawMessage `json:"review_plan"`
	ReviewPlanHash string          `json:"review_plan_hash"`
	ThreadIDs      []string        `json:"thread_ids"`
}

type AddressReviewEvidence struct {
	Head             string            `json:"head"`
	Commands         []CommandEvidence `json:"commands"`
	PushReceipt      Receipt           `json:"push_receipt"`
	ThreadReceipts   []ThreadReceipt   `json:"thread_receipts"`
	SourceRevisions  []SourceRevision  `json:"source_revisions"`
	WorkItemRevision string            `json:"work_item_revision"`
	Reconciled       bool              `json:"reconciled"`
}

type ResolveWorkItemEvidence struct {
	MergeReceipt      Receipt `json:"merge_receipt"`
	TrackerStatus     string  `json:"tracker_status"`
	TransitionReceipt Receipt `json:"transition_receipt"`
	Reconciled        bool    `json:"reconciled"`
}

type WakeEvidence struct {
	Scheduled bool   `json:"scheduled"`
	DueAt     string `json:"due_at"`
	Reason    string `json:"reason"`
}
