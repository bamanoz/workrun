package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bamanoz/workrun/internal/domain"
	"github.com/bamanoz/workrun/internal/pathsecurity"
	_ "modernc.org/sqlite"
	"runtime"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrLeaseHeld    = errors.New("run lease is held by another owner")
	ErrLeaseExpired = errors.New("lease is missing or expired")
	ErrConflict     = errors.New("concurrent state change")
	ErrDuplicateRun = errors.New("active run already exists")
)

const currentSchemaVersion = 3

type Store struct {
	db   *sql.DB
	path string
	now  func() time.Time
}

func DefaultPath() (string, error) {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "workrun", "workrun.db"), nil
	}
	if runtime.GOOS == "darwin" {
		if data, err := os.UserConfigDir(); err == nil {
			return filepath.Join(data, "workrun", "workrun.db"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "workrun", "workrun.db"), nil
}

func Open(path string) (*Store, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	dir := filepath.Dir(path)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	if err := pathsecurity.ValidateDir(dir, true); err != nil {
		return nil, fmt.Errorf("unsafe state directory: %w", err)
	}
	existed := false
	if _, err := os.Lstat(path); err == nil {
		existed = true
		if err = pathsecurity.ValidateFile(path); err != nil {
			return nil, fmt.Errorf("unsafe state database: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dsn := "file:" + url.PathEscape(path) + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if !existed {
		if err = db.Ping(); err != nil {
			db.Close()
			return nil, err
		}
		if err = os.Chmod(path, 0o600); err != nil {
			db.Close()
			return nil, fmt.Errorf("secure new database: %w", err)
		}
	}
	s := &Store{db: db, path: path, now: func() time.Time { return time.Now().UTC() }}
	var existingVersion int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&existingVersion); err != nil {
		db.Close()
		return nil, err
	}
	if existingVersion > currentSchemaVersion {
		db.Close()
		return nil, fmt.Errorf("database schema %d is newer than supported %d", existingVersion, currentSchemaVersion)
	}
	if existingVersion > 0 && existingVersion < currentSchemaVersion {
		if _, err = backupDatabase(db, path, existingVersion); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) SetClock(now func() time.Time) { s.now = now }

func secureExisting(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return pathsecurity.ValidateFile(path)
}

func backupDatabase(db *sql.DB, path string, version int) (string, error) {
	backup := fmt.Sprintf("%s.backup-v%d-%s", path, version, time.Now().UTC().Format("20060102T150405Z"))
	if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
		return "", fmt.Errorf("backup database before migration: %w", err)
	}
	if err := os.Chmod(backup, 0o600); err != nil {
		return "", err
	}
	return backup, nil
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version)
	return version, err
}

func (s *Store) IntegrityCheck(ctx context.Context) (string, error) {
	var result string
	err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result)
	return result, err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SecurityInfo() (pathsecurity.Info, pathsecurity.Info) {
	return pathsecurity.Inspect(s.path, false, true), pathsecurity.Inspect(filepath.Dir(s.path), true, true)
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workflow_snapshots(
  hash TEXT PRIMARY KEY, name TEXT NOT NULL, protocol TEXT NOT NULL, content BLOB NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS work_groups(
  id TEXT PRIMARY KEY, work_item_provider TEXT NOT NULL, work_item_id TEXT NOT NULL, work_item_key TEXT NOT NULL,
  supersedes TEXT, created_at TEXT NOT NULL, UNIQUE(work_item_provider,work_item_id)
);
CREATE TABLE IF NOT EXISTS manifest_snapshots(
  hash TEXT PRIMARY KEY, content BLOB NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS runs(
  id TEXT PRIMARY KEY, group_id TEXT NOT NULL REFERENCES work_groups(id), work_item_provider TEXT NOT NULL, work_item_id TEXT NOT NULL,
  work_item_key TEXT NOT NULL, repository TEXT NOT NULL, state TEXT NOT NULL, terminal INTEGER NOT NULL DEFAULT 0,
  workflow_hash TEXT NOT NULL REFERENCES workflow_snapshots(hash), version INTEGER NOT NULL, snapshot BLOB NOT NULL,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS runs_one_active ON runs(group_id) WHERE terminal = 0;

CREATE INDEX IF NOT EXISTS runs_state ON runs(state, updated_at);
CREATE TABLE IF NOT EXISTS events(
  id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(id), sequence INTEGER NOT NULL,
  type TEXT NOT NULL, from_state TEXT, to_state TEXT, action_id TEXT, data BLOB, created_at TEXT NOT NULL,
  UNIQUE(run_id, sequence)
);
CREATE TABLE IF NOT EXISTS briefs(
  run_id TEXT NOT NULL REFERENCES runs(id), hash TEXT NOT NULL, content BLOB NOT NULL, created_at TEXT NOT NULL,
  PRIMARY KEY(run_id, hash)
);
CREATE TABLE IF NOT EXISTS artifacts(
  run_id TEXT NOT NULL REFERENCES runs(id), kind TEXT NOT NULL, hash TEXT NOT NULL, content BLOB, created_at TEXT NOT NULL,
  PRIMARY KEY(run_id,kind,hash)
);
CREATE TABLE IF NOT EXISTS requirement_sources(
  run_id TEXT NOT NULL REFERENCES runs(id), role TEXT NOT NULL, external_id TEXT NOT NULL,
  revision TEXT NOT NULL, metadata BLOB, created_at TEXT NOT NULL,
  PRIMARY KEY(run_id, role, external_id)
);
CREATE TABLE IF NOT EXISTS approvals(
  run_id TEXT NOT NULL REFERENCES runs(id), kind TEXT NOT NULL, artifact_hash TEXT NOT NULL,
  approved_by TEXT NOT NULL, approved_at TEXT NOT NULL, PRIMARY KEY(run_id, kind, artifact_hash)
);
CREATE TABLE IF NOT EXISTS leases(
  run_id TEXT PRIMARY KEY REFERENCES runs(id), token TEXT NOT NULL, owner TEXT NOT NULL, expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS actions(
  id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), nonce TEXT NOT NULL UNIQUE, type TEXT NOT NULL,
  state TEXT NOT NULL, lease_token TEXT NOT NULL, envelope BLOB NOT NULL, result BLOB,
  created_at TEXT NOT NULL, completed_at TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS actions_one_pending ON actions(run_id) WHERE state = 'pending';
CREATE TABLE IF NOT EXISTS schedules(
  run_id TEXT PRIMARY KEY REFERENCES runs(id), due_at TEXT NOT NULL, reason TEXT NOT NULL, attempt INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS proposals(
  id TEXT PRIMARY KEY, run_id TEXT, scope TEXT NOT NULL, reason TEXT NOT NULL, artifact_hash TEXT NOT NULL,
  diff BLOB NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL, approved_at TEXT
);
CREATE TABLE IF NOT EXISTS change_requests(
  run_id TEXT PRIMARY KEY REFERENCES runs(id), provider TEXT NOT NULL, external_id TEXT NOT NULL,
  state TEXT NOT NULL, snapshot BLOB NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS review_events(
  run_id TEXT NOT NULL REFERENCES runs(id), fingerprint TEXT NOT NULL, remote_id TEXT, type TEXT NOT NULL,
  payload BLOB, observed_at TEXT NOT NULL, PRIMARY KEY(run_id, fingerprint)
);
CREATE TABLE IF NOT EXISTS provider_cursors(
  run_id TEXT NOT NULL REFERENCES runs(id), capability TEXT NOT NULL, cursor TEXT NOT NULL, updated_at TEXT NOT NULL,
  PRIMARY KEY(run_id, capability)
);
INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES(1, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate database v1: %w", err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 0 {
		if _, err := s.db.ExecContext(ctx, `PRAGMA user_version=1`); err != nil {
			return err
		}
		version = 1
	}
	if version < 2 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS artifacts(run_id TEXT NOT NULL REFERENCES runs(id),kind TEXT NOT NULL,hash TEXT NOT NULL,content BLOB,created_at TEXT NOT NULL,PRIMARY KEY(run_id,kind,hash));INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(2,strftime('%Y-%m-%dT%H:%M:%fZ','now'));PRAGMA user_version=2;`); err != nil {
			return fmt.Errorf("migrate database v2: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	if version < 3 {
		var conflictingGroup string
		err := s.db.QueryRowContext(ctx, `SELECT group_id FROM runs WHERE terminal=0 GROUP BY group_id HAVING COUNT(*)>1 LIMIT 1`).Scan(&conflictingGroup)
		if err == nil {
			return fmt.Errorf("migrate database v3: work group %s has multiple active repository runs; resolve the conflict before upgrading", conflictingGroup)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `DROP INDEX IF EXISTS runs_one_active;CREATE UNIQUE INDEX runs_one_active ON runs(group_id) WHERE terminal=0;INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(3,strftime('%Y-%m-%dT%H:%M:%fZ','now'));PRAGMA user_version=3;`); err != nil {
			return fmt.Errorf("migrate database v3: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func NewID(prefix string) (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(buf[:]), nil
}

func (s *Store) SaveWorkflow(ctx context.Context, hash, name, protocol string, content []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO workflow_snapshots(hash,name,protocol,content,created_at) VALUES(?,?,?,?,?)`, hash, name, protocol, content, formatTime(s.now()))
	return err
}

func (s *Store) GetWorkflow(ctx context.Context, hash string) ([]byte, error) {
	var content []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM workflow_snapshots WHERE hash=?`, hash).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return content, err
}

func (s *Store) SaveManifest(ctx context.Context, hash string, content []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO manifest_snapshots(hash,content,created_at) VALUES(?,?,?)`, hash, content, formatTime(s.now()))
	return err
}

func (s *Store) GetManifest(ctx context.Context, hash string) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM manifest_snapshots WHERE hash=?`, hash).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return raw, err
}

type CreateRunInput struct {
	Provider, WorkItemID, WorkItemKey, Repository, WorkflowName, WorkflowHash, ManifestHash string
	Supersedes                                                                              string
}

func (s *Store) CreateOrResumeRun(ctx context.Context, in CreateRunInput) (*domain.Run, bool, error) {
	if in.Provider == "" || in.WorkItemID == "" || in.Repository == "" || in.WorkflowHash == "" {
		return nil, false, errors.New("provider, work item id, repository, and workflow hash are required")
	}
	repo, err := filepath.Abs(in.Repository)
	if err != nil {
		return nil, false, err
	}
	existing, err := s.findActive(ctx, in.Provider, in.WorkItemID)
	if err == nil {
		if existing.Repository == repo {
			return existing, false, nil
		}
		return nil, false, fmt.Errorf("%w: work item %s is active in repository %s", ErrDuplicateRun, in.WorkItemID, existing.Repository)
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	id, err := NewID("run")
	if err != nil {
		return nil, false, err
	}
	groupID := ""
	newGroup := false
	err = s.db.QueryRowContext(ctx, `SELECT id FROM work_groups WHERE work_item_provider=? AND work_item_id=?`, in.Provider, in.WorkItemID).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		groupID, err = NewID("group")
		newGroup = true
	}
	if err != nil {
		return nil, false, err
	}
	now := s.now()
	run := &domain.Run{ID: id, GroupID: groupID, WorkItemProvider: in.Provider, WorkItemID: in.WorkItemID,
		WorkItemKey: in.WorkItemKey, Repository: repo, State: domain.StateDiscoveringContext,
		WorkflowName: in.WorkflowName, WorkflowHash: in.WorkflowHash, ProtocolVersion: domain.ProtocolVersion,
		ManifestHash: in.ManifestHash, Version: 1, Supersedes: in.Supersedes, CreatedAt: now, UpdatedAt: now}
	snapshot, _ := json.Marshal(run)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if newGroup {
		_, err = tx.ExecContext(ctx, `INSERT INTO work_groups(id,work_item_provider,work_item_id,work_item_key,supersedes,created_at) VALUES(?,?,?,?,?,?)`, run.GroupID, run.WorkItemProvider, run.WorkItemID, run.WorkItemKey, nullString(run.Supersedes), formatTime(now))
		if err != nil {
			return nil, false, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runs(id,group_id,work_item_provider,work_item_id,work_item_key,repository,state,terminal,workflow_hash,version,snapshot,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.GroupID, run.WorkItemProvider, run.WorkItemID, run.WorkItemKey, run.Repository, run.State, 0, run.WorkflowHash, run.Version, snapshot, formatTime(now), formatTime(now))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			_ = tx.Rollback()
			if current, getErr := s.findActive(ctx, in.Provider, in.WorkItemID); getErr == nil {
				if current.Repository == repo {
					return current, false, nil
				}
				return nil, false, fmt.Errorf("%w: work item %s is active in repository %s", ErrDuplicateRun, in.WorkItemID, current.Repository)
			}
			return nil, false, ErrDuplicateRun
		}
		return nil, false, err
	}
	data, _ := json.Marshal(map[string]string{"workflow_hash": run.WorkflowHash})
	if err := appendEventTx(ctx, tx, run.ID, 1, "run_started", "", run.State, "", data, now); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return run, true, nil
}

func (s *Store) findActive(ctx context.Context, provider, itemID string) (*domain.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT snapshot FROM runs WHERE work_item_provider=? AND work_item_id=? AND terminal=0`, provider, itemID)
	return scanRun(row)
}

func (s *Store) GetRun(ctx context.Context, id string) (*domain.Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT snapshot FROM runs WHERE id=?`, id))
}

type scannable interface{ Scan(...any) error }

func scanRun(row scannable) (*domain.Run, error) {
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var run domain.Run
	if err := json.Unmarshal(raw, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *Store) ListRuns(ctx context.Context, activeOnly, dueOnly bool) ([]domain.Run, error) {
	q := `SELECT r.snapshot FROM runs r`
	var where []string
	var args []any
	if dueOnly {
		q += ` JOIN schedules sc ON sc.run_id=r.id`
		where = append(where, `sc.due_at <= ?`)
		args = append(args, formatTime(s.now()))
	}
	if activeOnly {
		where = append(where, `r.terminal=0`)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY r.updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *run)
	}
	return result, rows.Err()
}

func (s *Store) Events(ctx context.Context, runID string) ([]domain.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,run_id,sequence,type,from_state,to_state,action_id,data,created_at FROM events WHERE run_id=? ORDER BY sequence`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var e domain.Event
		var from, to, action sql.NullString
		var raw []byte
		var created string
		if err := rows.Scan(&e.ID, &e.RunID, &e.Sequence, &e.Type, &from, &to, &action, &raw, &created); err != nil {
			return nil, err
		}
		e.FromState = domain.State(from.String)
		e.ToState = domain.State(to.String)
		e.ActionID = action.String
		e.Data = raw
		e.CreatedAt = parseTime(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) UpdateRun(ctx context.Context, run *domain.Run, eventType, actionID string, data any) error {
	current, err := s.GetRun(ctx, run.ID)
	if err != nil {
		return err
	}
	from := current.State
	now := s.now()
	run.UpdatedAt = now
	run.Version = current.Version + 1
	snapshot, _ := json.Marshal(run)
	eventData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE runs SET state=?,terminal=?,workflow_hash=?,version=?,snapshot=?,updated_at=? WHERE id=? AND version=?`, run.State, boolInt(run.State.Terminal()), run.WorkflowHash, run.Version, snapshot, formatTime(now), run.ID, current.Version)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	if err := appendEventTx(ctx, tx, run.ID, run.Version, eventType, from, run.State, actionID, eventData, now); err != nil {
		return err
	}
	if run.NextWakeAt == nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE run_id=?`, run.ID); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO schedules(run_id,due_at,reason,attempt) VALUES(?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET due_at=excluded.due_at,reason=excluded.reason,attempt=excluded.attempt`, run.ID, formatTime(*run.NextWakeAt), run.WaitReason, run.PollAttempt); err != nil {
			return err
		}
	}
	if run.State == domain.StatePaused || run.State.Terminal() || run.WaitKind != domain.WaitNone {
		if _, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE run_id=?`, run.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func appendEventTx(ctx context.Context, tx *sql.Tx, runID string, seq int64, typ string, from, to domain.State, actionID string, data []byte, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events(run_id,sequence,type,from_state,to_state,action_id,data,created_at) VALUES(?,?,?,?,?,?,?,?)`, runID, seq, typ, nullString(string(from)), nullString(string(to)), nullString(actionID), data, formatTime(now))
	return err
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(s string) time.Time  { t, _ := time.Parse(time.RFC3339Nano, s); return t }
