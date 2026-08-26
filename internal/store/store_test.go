package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bamanoz/workrun/internal/domain"
)

func testStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	return s, &now
}

func seedRun(t *testing.T, s *Store) *domain.Run {
	t.Helper()
	ctx := context.Background()
	if err := s.SaveWorkflow(ctx, "hash", "tracked-change", ">=1.0 <2.0", []byte("workflow")); err != nil {
		t.Fatal(err)
	}
	run, created, err := s.CreateOrResumeRun(ctx, CreateRunInput{Provider: "tracker", WorkItemID: "ABC-1", WorkItemKey: "ABC-1", Repository: t.TempDir(), WorkflowName: "tracked-change", WorkflowHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected new run")
	}
	return run
}

func TestCreateOrResumeKeepsOneActiveRun(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	run := seedRun(t, s)
	again, created, err := s.CreateOrResumeRun(ctx, CreateRunInput{Provider: run.WorkItemProvider, WorkItemID: run.WorkItemID, WorkItemKey: run.WorkItemKey, Repository: run.Repository, WorkflowName: run.WorkflowName, WorkflowHash: run.WorkflowHash})
	if err != nil {
		t.Fatal(err)
	}
	if created || again.ID != run.ID {
		t.Fatalf("expected resume of %s, got %#v", run.ID, again)
	}
}

func TestActiveWorkItemRejectsSecondRepository(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	first := seedRun(t, s)
	_, created, err := s.CreateOrResumeRun(ctx, CreateRunInput{Provider: first.WorkItemProvider, WorkItemID: first.WorkItemID, WorkItemKey: first.WorkItemKey, Repository: t.TempDir(), WorkflowName: first.WorkflowName, WorkflowHash: first.WorkflowHash})
	if !errors.Is(err, ErrDuplicateRun) || created {
		t.Fatalf("second active repository accepted: created=%v err=%v", created, err)
	}
}

func TestLeaseExpiryAndTakeover(t *testing.T) {
	s, now := testStore(t)
	ctx := context.Background()
	run := seedRun(t, s)
	first, err := s.AcquireLease(ctx, run.ID, "session-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireLease(ctx, run.ID, "session-b", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("expected lease held, got %v", err)
	}
	*now = now.Add(2 * time.Minute)
	second, err := s.AcquireLease(ctx, run.ID, "session-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Token == first.Token {
		t.Fatal("takeover must issue a new token")
	}
	if _, err = s.ValidateLease(ctx, run.ID, first.Token); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("old lease accepted: %v", err)
	}
}

func TestLeaseRenewalExtendsOwnership(t *testing.T) {
	s, now := testStore(t)
	ctx := context.Background()
	run := seedRun(t, s)
	lease, err := s.AcquireLease(ctx, run.ID, "session", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(30 * time.Second)
	renewed, err := s.RenewLease(ctx, run.ID, lease.Token, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(90 * time.Second)
	if _, err = s.ValidateLease(ctx, run.ID, renewed.Token); err != nil {
		t.Fatalf("renewed lease expired early: %v", err)
	}
}

func TestLeaseTokenCannotBeRotatedOrReleasedByAnotherCaller(t *testing.T) {
	s, now := testStore(t)
	ctx := context.Background()
	run := seedRun(t, s)
	if _, err := s.AcquireLease(ctx, run.ID, "", time.Minute); err == nil {
		t.Fatal("empty lease owner accepted")
	}
	lease, err := s.AcquireLease(ctx, run.ID, "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireLease(ctx, run.ID, "session", time.Hour); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("same owner rotated live token: %v", err)
	}
	if err = s.ReleaseLease(ctx, run.ID, "wrong-token"); err != nil {
		t.Fatalf("wrong token release failed idempotency: %v", err)
	}
	if _, err = s.ValidateLease(ctx, run.ID, lease.Token); err != nil {
		t.Fatalf("wrong token removed lease: %v", err)
	}
	*now = now.Add(time.Minute)
	renewed, err := s.RenewLease(ctx, run.ID, lease.Token, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.ExpiresAt.Equal(lease.ExpiresAt) {
		t.Fatalf("short renewal reduced expiry: got %s want %s", renewed.ExpiresAt, lease.ExpiresAt)
	}
	if _, err = s.RenewLease(ctx, run.ID, lease.Token, 0); err == nil {
		t.Fatal("non-positive renewal accepted")
	}
	if err = s.ReleaseLease(ctx, run.ID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ValidateLease(ctx, run.ID, lease.Token); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("released lease remained valid: %v", err)
	}
}

func TestActionCompletionAndTransitionAreAtomicAndIdempotent(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	run := seedRun(t, s)
	lease, err := s.AcquireLease(ctx, run.ID, "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	action := domain.ActionEnvelope{ProtocolVersion: domain.ProtocolVersion, SchemaVersion: 1, RunID: run.ID, ActionID: "action-1", Nonce: "nonce-1", Lease: *lease, Type: "inspect_context"}
	if err = s.CreateAction(ctx, action); err != nil {
		t.Fatal(err)
	}
	result := domain.ActionResult{ProtocolVersion: domain.ProtocolVersion, RunID: run.ID, ActionID: action.ActionID, Nonce: action.Nonce, LeaseToken: lease.Token, Outcome: "complete", Evidence: json.RawMessage(`{"ok":true}`)}
	run.State = domain.StateAwaitingBriefApproval
	applied, err := s.CommitActionResult(ctx, result, run, "action_completed")
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("first completion not applied")
	}
	again, err := s.CommitActionResult(ctx, result, run, "action_completed")
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("duplicate completion applied")
	}
	stored, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.StateAwaitingBriefApproval {
		t.Fatalf("state=%s", stored.State)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events=%d, want start+completion", len(events))
	}
}

func TestRetentionPurgesBulkyTerminalPayloads(t *testing.T) {
	s, now := testStore(t)
	ctx := context.Background()
	run := seedRun(t, s)
	brief := domain.Brief{RequirementSources: []domain.RequirementSource{{Role: "business", ExternalID: "B-1", Revision: "1", Summary: "sensitive"}}}
	if err := s.SaveBrief(ctx, run.ID, "brief", brief); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveReviewEvents(ctx, run.ID, []json.RawMessage{json.RawMessage(`{"id":"r1","type":"comment","body":"sensitive"}`)}); err != nil {
		t.Fatal(err)
	}
	run.State = domain.StateCancelled
	if err := s.UpdateRun(ctx, run, "cancelled", "", nil); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(31 * 24 * time.Hour)
	count, err := s.PurgeExpiredPayloads(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("purged=%d want=2", count)
	}
}

func TestOpenBacksUpBeforeVersionedMigration(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	version, err := s.SchemaVersion(context.Background())
	if err != nil || version != currentSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	backups, err := filepath.Glob(path + ".backup-v1-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups=%v err=%v", backups, err)
	}
}

func TestPendingActionSurvivesStoreRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	run := seedRun(t, s)
	lease, err := s.AcquireLease(ctx, run.ID, "host", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	action := domain.ActionEnvelope{ProtocolVersion: domain.ProtocolVersion, SchemaVersion: 1, RunID: run.ID, ActionID: "action-crash", Nonce: "nonce-crash", Lease: *lease, Type: "publish_change", Inputs: json.RawMessage(`{}`)}
	if err = s.CreateAction(ctx, action); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	pending, err := reopened.PendingAction(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.ActionID != action.ActionID || pending.Nonce != action.Nonce {
		t.Fatalf("pending action changed after restart: %#v", pending)
	}
}
