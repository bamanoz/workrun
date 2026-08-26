package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bamanoz/workrun/internal/domain"
)

func (s *Store) SaveBrief(ctx context.Context, runID, hash string, brief domain.Brief) error {
	raw, err := json.Marshal(brief)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(s.now())
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO briefs(run_id,hash,content,created_at) VALUES(?,?,?,?)`, runID, hash, raw, now); err != nil {
		return err
	}
	for _, source := range brief.RequirementSources {
		metadata, _ := json.Marshal(map[string]any{"provider": source.Provider, "url": source.URL, "title": source.Title, "summary": source.Summary, "user_supplied": source.UserSupplied})
		if _, err = tx.ExecContext(ctx, `INSERT INTO requirement_sources(run_id,role,external_id,revision,metadata,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,role,external_id) DO UPDATE SET revision=excluded.revision,metadata=excluded.metadata`, runID, source.Role, source.ExternalID, source.Revision, metadata, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SaveArtifact(ctx context.Context, runID, kind, hash string, content json.RawMessage) error {
	if runID == "" || kind == "" || hash == "" || len(content) == 0 {
		return errors.New("artifact run, kind, hash, and content are required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO artifacts(run_id,kind,hash,content,created_at) VALUES(?,?,?,?,?)`, runID, kind, hash, content, formatTime(s.now()))
	return err
}

func (s *Store) GetArtifact(ctx context.Context, runID, kind, hash string) (json.RawMessage, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM artifacts WHERE run_id=? AND kind=? AND hash=?`, runID, kind, hash).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, ErrNotFound
	}
	return raw, nil
}

func (s *Store) SaveChangeRequest(ctx context.Context, runID string, cr domain.ChangeRequest) error {
	raw, err := json.Marshal(cr)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO change_requests(run_id,provider,external_id,state,snapshot,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET provider=excluded.provider,external_id=excluded.external_id,state=excluded.state,snapshot=excluded.snapshot,updated_at=excluded.updated_at`, runID, cr.Provider, cr.ExternalID, cr.State, raw, formatTime(s.now()))
	return err
}

func (s *Store) SaveReviewEvents(ctx context.Context, runID string, events []json.RawMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, event := range events {
		var fields map[string]any
		if err = json.Unmarshal(event, &fields); err != nil {
			return err
		}
		remoteID, _ := fields["id"].(string)
		typ, _ := fields["type"].(string)
		fingerprint, _ := fields["fingerprint"].(string)
		if fingerprint == "" {
			fingerprint = remoteID
		}
		if fingerprint == "" {
			fingerprint, err = hashJSON(event)
			if err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO review_events(run_id,fingerprint,remote_id,type,payload,observed_at) VALUES(?,?,?,?,?,?)`, runID, fingerprint, nullString(remoteID), typ, event, formatTime(s.now())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func hashJSON(raw []byte) (string, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Store) GetBrief(ctx context.Context, runID, hash string) (*domain.Brief, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM briefs WHERE run_id=? AND hash=?`, runID, hash).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var brief domain.Brief
	if err := json.Unmarshal(raw, &brief); err != nil {
		return nil, err
	}
	return &brief, nil
}

func (s *Store) CommitApproval(ctx context.Context, run *domain.Run, from domain.State, kind, hash, approvedBy string) error {
	if hash == "" || approvedBy == "" {
		return errors.New("artifact hash and approver are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentRaw []byte
	var currentVersion int64
	if err = tx.QueryRowContext(ctx, `SELECT snapshot,version FROM runs WHERE id=?`, run.ID).Scan(&currentRaw, &currentVersion); err != nil {
		return err
	}
	var current domain.Run
	if err = json.Unmarshal(currentRaw, &current); err != nil {
		return err
	}
	if current.State != from {
		return ErrConflict
	}
	now := s.now()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO approvals(run_id,kind,artifact_hash,approved_by,approved_at) VALUES(?,?,?,?,?)`, run.ID, kind, hash, approvedBy, formatTime(now)); err != nil {
		return err
	}
	run.Version = currentVersion + 1
	run.UpdatedAt = now
	snapshot, err := json.Marshal(run)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE runs SET state=?,terminal=?,workflow_hash=?,version=?,snapshot=?,updated_at=? WHERE id=? AND version=?`, run.State, boolInt(run.State.Terminal()), run.WorkflowHash, run.Version, snapshot, formatTime(now), run.ID, currentVersion)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	data, _ := json.Marshal(map[string]string{"kind": kind, "hash": hash, "actor": approvedBy})
	if err = appendEventTx(ctx, tx, run.ID, run.Version, "artifact_approved", from, run.State, "", data, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM schedules WHERE run_id=?`, run.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AcquireLease(ctx context.Context, runID, owner string, ttl time.Duration) (*domain.Lease, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("lease owner is required")
	}
	if ttl <= 0 {
		return nil, errors.New("lease TTL must be positive")
	}
	now := s.now()
	expires := now.Add(ttl)
	token, err := NewID("lease")
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var currentToken, currentOwner, currentExpiry string
	err = tx.QueryRowContext(ctx, `SELECT token,owner,expires_at FROM leases WHERE run_id=?`, runID).Scan(&currentToken, &currentOwner, &currentExpiry)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && parseTime(currentExpiry).After(now) {
		return nil, fmt.Errorf("%w: owner=%s expires_at=%s", ErrLeaseHeld, currentOwner, currentExpiry)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO leases(run_id,token,owner,expires_at) VALUES(?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET token=excluded.token,owner=excluded.owner,expires_at=excluded.expires_at`, runID, token, owner, formatTime(expires))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &domain.Lease{RunID: runID, Token: token, Owner: owner, ExpiresAt: expires}, nil
}

func (s *Store) RenewLease(ctx context.Context, runID, token string, ttl time.Duration) (*domain.Lease, error) {
	if ttl <= 0 {
		return nil, errors.New("lease TTL must be positive")
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var owner, rawExpiry string
	err = tx.QueryRowContext(ctx, `SELECT owner,expires_at FROM leases WHERE run_id=? AND token=?`, runID, token).Scan(&owner, &rawExpiry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLeaseExpired
	}
	if err != nil {
		return nil, err
	}
	current := parseTime(rawExpiry)
	if !current.After(now) {
		return nil, ErrLeaseExpired
	}
	expires := now.Add(ttl)
	if expires.Before(current) {
		expires = current
	}
	res, err := tx.ExecContext(ctx, `UPDATE leases SET expires_at=? WHERE run_id=? AND token=? AND expires_at=?`, formatTime(expires), runID, token, rawExpiry)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrLeaseExpired
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &domain.Lease{RunID: runID, Token: token, Owner: owner, ExpiresAt: expires}, nil
}

func (s *Store) ValidateLease(ctx context.Context, runID, token string) (*domain.Lease, error) {
	var owner, expires string
	err := s.db.QueryRowContext(ctx, `SELECT owner,expires_at FROM leases WHERE run_id=? AND token=?`, runID, token).Scan(&owner, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLeaseExpired
	}
	if err != nil {
		return nil, err
	}
	exp := parseTime(expires)
	if !exp.After(s.now()) {
		return nil, ErrLeaseExpired
	}
	return &domain.Lease{RunID: runID, Token: token, Owner: owner, ExpiresAt: exp}, nil
}

func (s *Store) LeaseSummary(ctx context.Context, runID string) (string, *time.Time, error) {
	var owner, expires string
	err := s.db.QueryRowContext(ctx, `SELECT owner,expires_at FROM leases WHERE run_id=?`, runID).Scan(&owner, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	value := parseTime(expires)
	return owner, &value, nil
}

func (s *Store) ReleaseLease(ctx context.Context, runID, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE run_id=? AND token=?`, runID, token)
	return err
}

func (s *Store) PendingAction(ctx context.Context, runID string) (*domain.ActionEnvelope, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT envelope FROM actions WHERE run_id=? AND state='pending'`, runID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var a domain.ActionEnvelope
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) CreateAction(ctx context.Context, action domain.ActionEnvelope) error {
	raw, err := json.Marshal(action)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO actions(id,run_id,nonce,type,state,lease_token,envelope,created_at) VALUES(?,?,?,?,?,?,?,?)`, action.ActionID, action.RunID, action.Nonce, action.Type, "pending", action.Lease.Token, raw, formatTime(s.now()))
	return err
}

func (s *Store) FailPendingAction(ctx context.Context, runID, actionID, leaseToken string, result domain.ActionResult) error {
	if _, err := s.ValidateLease(ctx, runID, leaseToken); err != nil {
		return err
	}
	raw, _ := json.Marshal(result)
	res, err := s.db.ExecContext(ctx, `UPDATE actions SET state='failed',result=?,completed_at=? WHERE id=? AND run_id=? AND lease_token=? AND state='pending'`, raw, formatTime(s.now()), actionID, runID, leaseToken)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) PutCursor(ctx context.Context, runID, capability, cursor string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO provider_cursors(run_id,capability,cursor,updated_at) VALUES(?,?,?,?) ON CONFLICT(run_id,capability) DO UPDATE SET cursor=excluded.cursor,updated_at=excluded.updated_at`, runID, capability, cursor, formatTime(s.now()))
	return err
}

func (s *Store) GetCursor(ctx context.Context, runID, capability string) (string, error) {
	var cursor string
	err := s.db.QueryRowContext(ctx, `SELECT cursor FROM provider_cursors WHERE run_id=? AND capability=?`, runID, capability).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return cursor, err
}

func (s *Store) CreateProposal(ctx context.Context, p *domain.Proposal) error {
	if p.Scope != "run" && p.Scope != "repository" && p.Scope != "global" {
		return errors.New("proposal scope must be run, repository, or global")
	}
	if strings.TrimSpace(p.Reason) == "" || p.ArtifactHash == "" || !json.Valid(p.Diff) {
		return errors.New("proposal reason, artifact hash, and valid JSON diff are required")
	}
	if p.ID == "" {
		id, err := NewID("proposal")
		if err != nil {
			return err
		}
		p.ID = id
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = s.now()
	}
	if p.Status == "" {
		p.Status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO proposals(id,run_id,scope,reason,artifact_hash,diff,status,created_at) VALUES(?,?,?,?,?,?,?,?)`, p.ID, nullString(p.RunID), p.Scope, p.Reason, p.ArtifactHash, []byte(p.Diff), p.Status, formatTime(p.CreatedAt))
	return err
}

func (s *Store) ListProposals(ctx context.Context) ([]domain.Proposal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(run_id,''),scope,reason,artifact_hash,diff,status,created_at,approved_at FROM proposals ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Proposal
	for rows.Next() {
		var p domain.Proposal
		var created string
		var approved sql.NullString
		if err := rows.Scan(&p.ID, &p.RunID, &p.Scope, &p.Reason, &p.ArtifactHash, &p.Diff, &p.Status, &created, &approved); err != nil {
			return nil, err
		}
		p.CreatedAt = parseTime(created)
		if approved.Valid {
			t := parseTime(approved.String)
			p.ApprovedAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProposal(ctx context.Context, id string) (*domain.Proposal, error) {
	var p domain.Proposal
	var created string
	var approved sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,COALESCE(run_id,''),scope,reason,artifact_hash,diff,status,created_at,approved_at FROM proposals WHERE id=?`, id).Scan(&p.ID, &p.RunID, &p.Scope, &p.Reason, &p.ArtifactHash, &p.Diff, &p.Status, &created, &approved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = parseTime(created)
	if approved.Valid {
		value := parseTime(approved.String)
		p.ApprovedAt = &value
	}
	return &p, nil
}

// CommitActionResult atomically records a unique completion, updates the run snapshot,
// and appends its transition event. A repeated completion returns applied=false.
func (s *Store) CommitActionResult(ctx context.Context, result domain.ActionResult, run *domain.Run, eventType string) (bool, error) {
	return s.commitAction(ctx, result, run, eventType, "completed")
}

func (s *Store) CommitActionFailure(ctx context.Context, result domain.ActionResult, run *domain.Run, eventType string) (bool, error) {
	return s.commitAction(ctx, result, run, eventType, "failed")
}

func (s *Store) commitAction(ctx context.Context, result domain.ActionResult, run *domain.Run, eventType, finalActionState string) (bool, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var owner, expires string
	err = tx.QueryRowContext(ctx, `SELECT owner,expires_at FROM leases WHERE run_id=? AND token=?`, result.RunID, result.LeaseToken).Scan(&owner, &expires)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !parseTime(expires).After(now) {
		return false, ErrLeaseExpired
	}
	if err != nil {
		return false, err
	}
	var actionState, nonce, leaseToken, actionType string
	err = tx.QueryRowContext(ctx, `SELECT state,nonce,lease_token,type FROM actions WHERE id=? AND run_id=?`, result.ActionID, result.RunID).Scan(&actionState, &nonce, &leaseToken, &actionType)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if actionState == finalActionState {
		return false, nil
	}
	if actionState != "pending" || nonce != result.Nonce || leaseToken != result.LeaseToken {
		return false, ErrConflict
	}
	var currentRaw []byte
	var currentVersion int64
	if err = tx.QueryRowContext(ctx, `SELECT snapshot,version FROM runs WHERE id=?`, run.ID).Scan(&currentRaw, &currentVersion); err != nil {
		return false, err
	}
	var current domain.Run
	if err = json.Unmarshal(currentRaw, &current); err != nil {
		return false, err
	}
	run.Version = currentVersion + 1
	run.UpdatedAt = now
	snapshot, err := json.Marshal(run)
	if err != nil {
		return false, err
	}
	resultRaw, err := json.Marshal(result)
	if err != nil {
		return false, err
	}
	if finalActionState == "completed" {
		if err = persistActionEvidenceTx(ctx, tx, actionType, result.Evidence, run, now); err != nil {
			return false, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE actions SET state=?,result=?,completed_at=? WHERE id=? AND state='pending'`, finalActionState, resultRaw, formatTime(now), result.ActionID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return false, ErrConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE runs SET state=?,terminal=?,workflow_hash=?,version=?,snapshot=?,updated_at=? WHERE id=? AND version=?`, run.State, boolInt(run.State.Terminal()), run.WorkflowHash, run.Version, snapshot, formatTime(now), run.ID, currentVersion)
	if err != nil {
		return false, err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return false, ErrConflict
	}
	eventData, _ := json.Marshal(map[string]any{"outcome": result.Outcome, "summary": result.Summary, "evidence": json.RawMessage(result.Evidence)})
	if err = appendEventTx(ctx, tx, run.ID, run.Version, eventType, current.State, run.State, result.ActionID, eventData, now); err != nil {
		return false, err
	}
	if run.NextWakeAt == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM schedules WHERE run_id=?`, run.ID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO schedules(run_id,due_at,reason,attempt) VALUES(?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET due_at=excluded.due_at,reason=excluded.reason,attempt=excluded.attempt`, run.ID, formatTime(*run.NextWakeAt), run.WaitReason, run.PollAttempt)
	}
	if err != nil {
		return false, err
	}
	if run.State.Terminal() || run.WaitKind != domain.WaitNone {
		if _, err = tx.ExecContext(ctx, `DELETE FROM leases WHERE run_id=?`, run.ID); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func persistActionEvidenceTx(ctx context.Context, tx *sql.Tx, actionType string, raw json.RawMessage, run *domain.Run, now time.Time) error {
	if len(raw) == 0 {
		return nil
	}
	switch actionType {
	case "inspect_context":
		var evidence domain.InspectContextEvidence
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return err
		}
		briefRaw, err := json.Marshal(evidence.Brief)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO briefs(run_id,hash,content,created_at) VALUES(?,?,?,?)`, run.ID, evidence.BriefHash, briefRaw, formatTime(now)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM requirement_sources WHERE run_id=?`, run.ID); err != nil {
			return err
		}
		for _, source := range evidence.Brief.RequirementSources {
			metadata, _ := json.Marshal(map[string]any{"provider": source.Provider, "url": source.URL, "title": source.Title, "summary": source.Summary, "user_supplied": source.UserSupplied})
			if _, err = tx.ExecContext(ctx, `INSERT INTO requirement_sources(run_id,role,external_id,revision,metadata,created_at) VALUES(?,?,?,?,?,?)`, run.ID, source.Role, source.ExternalID, source.Revision, metadata, formatTime(now)); err != nil {
				return err
			}
		}
	case "analyze_review":
		var evidence domain.AnalyzeReviewEvidence
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO artifacts(run_id,kind,hash,content,created_at) VALUES(?,?,?,?,?)`, run.ID, "review_plan", evidence.ReviewPlanHash, evidence.ReviewPlan, formatTime(now)); err != nil {
			return err
		}
	case "poll_change_request":
		var evidence domain.PollEvidence
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return err
		}
		for _, event := range evidence.Events {
			eventRaw, err := json.Marshal(event)
			if err != nil {
				return err
			}
			fingerprint := event.Fingerprint
			if fingerprint == "" {
				fingerprint = event.ID
			}
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO review_events(run_id,fingerprint,remote_id,type,payload,observed_at) VALUES(?,?,?,?,?,?)`, run.ID, fingerprint, nullString(event.ID), event.Type, eventRaw, formatTime(now)); err != nil {
				return err
			}
		}
		if evidence.Cursor != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO provider_cursors(run_id,capability,cursor,updated_at) VALUES(?,?,?,?) ON CONFLICT(run_id,capability) DO UPDATE SET cursor=excluded.cursor,updated_at=excluded.updated_at`, run.ID, "change_request.poll", evidence.Cursor, formatTime(now)); err != nil {
				return err
			}
		}
	}
	if run.ChangeRequest != nil && (actionType == "publish_change" || actionType == "poll_change_request" || actionType == "reconcile" || actionType == "resolve_work_item") {
		snapshot, err := json.Marshal(run.ChangeRequest)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO change_requests(run_id,provider,external_id,state,snapshot,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET provider=excluded.provider,external_id=excluded.external_id,state=excluded.state,snapshot=excluded.snapshot,updated_at=excluded.updated_at`, run.ID, run.ChangeRequest.Provider, run.ChangeRequest.ExternalID, run.ChangeRequest.State, snapshot, formatTime(now)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RebindPendingAction(ctx context.Context, action domain.ActionEnvelope) error {
	raw, err := json.Marshal(action)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE actions SET lease_token=?,envelope=? WHERE id=? AND run_id=? AND state='pending'`, action.Lease.Token, raw, action.ActionID, action.RunID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return nil
}
func (s *Store) ApproveProposal(ctx context.Context, id, hash string) error {
	var expected, status string
	err := s.db.QueryRowContext(ctx, `SELECT artifact_hash,status FROM proposals WHERE id=?`, id).Scan(&expected, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	hash = strings.TrimSpace(hash)
	if status != "pending" || len(hash) < 8 || !strings.HasPrefix(expected, hash) {
		return ErrConflict
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx, `UPDATE proposals SET status='approved',approved_at=? WHERE id=? AND artifact_hash=? AND status='pending'`, formatTime(now), id, expected)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) PurgeExpiredPayloads(ctx context.Context, ttl time.Duration) (int64, error) {
	if ttl < 0 {
		return 0, errors.New("retention TTL cannot be negative")
	}
	cutoff := formatTime(s.now().Add(-ttl))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	queries := []string{
		`UPDATE requirement_sources SET metadata=NULL WHERE metadata IS NOT NULL AND run_id IN (SELECT id FROM runs WHERE terminal=1 AND updated_at<=?)`,
		`UPDATE review_events SET payload=NULL WHERE payload IS NOT NULL AND run_id IN (SELECT id FROM runs WHERE terminal=1 AND updated_at<=?)`,
		`UPDATE artifacts SET content=NULL WHERE content IS NOT NULL AND run_id IN (SELECT id FROM runs WHERE terminal=1 AND updated_at<=?)`,
		`UPDATE actions SET result=NULL WHERE result IS NOT NULL AND type IN ('inspect_context','poll_change_request','analyze_review') AND run_id IN (SELECT id FROM runs WHERE terminal=1 AND updated_at<=?)`,
		`UPDATE events SET data=NULL WHERE data IS NOT NULL AND action_id IN (SELECT id FROM actions WHERE type IN ('inspect_context','poll_change_request','analyze_review')) AND run_id IN (SELECT id FROM runs WHERE terminal=1 AND updated_at<=?)`,
	}
	var total int64
	for _, query := range queries {
		res, execErr := tx.ExecContext(ctx, query, cutoff)
		if execErr != nil {
			return 0, execErr
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}
