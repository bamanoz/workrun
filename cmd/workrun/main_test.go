package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bamanoz/workrun/internal/assets"
	"github.com/bamanoz/workrun/internal/config"
	"github.com/bamanoz/workrun/internal/domain"
	"github.com/bamanoz/workrun/internal/engine"
	"github.com/bamanoz/workrun/internal/store"
	"github.com/bamanoz/workrun/internal/workflow"
	"go.yaml.in/yaml/v3"
)

func TestInitRepositoryRequiresHashBoundApproval(t *testing.T) {
	dir := t.TempDir()
	evidence := `{"target_branch":"develop","in_review_intent":"review","resolved_intent":"resolved","verification_policy":"discover_from_repository","base_update_strategy":"repository_policy","sources":[{"path":"go.mod","revision":"abc","finding":"Go repository"}]}`
	if err := initRepository([]string{"--evidence", evidence, dir}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".agent-workflow.yaml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("manifest written before approval: %v", err)
	}
	var parsed bootstrapEvidence
	if err := json.Unmarshal([]byte(evidence), &parsed); err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(repositoryConfigFromEvidence(parsed))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.Hash(raw), "48d2cc2158a4e6f33e794abbc2e9720cf2d2e8b479692854673989d9c2909d95"; got != want {
		t.Fatalf("repository manifest golden changed: %s", got)
	}
}

func TestInstallAgentGeneratesBothHostWrappers(t *testing.T) {
	expected := map[string]string{"qwen": "41280512a32445746f7613db6202582a7c86e6f8c27a23eb831a0df0bc5a5d93", "omp": "ee188e34ce35a04d45efe30a91651f458f7130a1f10109d29a81142bc8136048"}
	for _, host := range []string{"qwen", "omp"} {
		dest := filepath.Join(t.TempDir(), host)
		if err := installAgent([]string{"--dest", dest, host}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dest, "workrun", "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}

		if got := config.Hash(raw); got != expected[host] {
			t.Fatalf("wrapper %s golden changed: %s", host, got)
		}
		text := string(raw)
		if !contains(text, "--tool-schema") || contains(text, "# inspect_context") {
			t.Fatalf("wrapper %s is not lazy", host)
		}
		if host == "qwen" && !contains(text, "one-shot wake") || host == "omp" && !contains(text, "optional session focus") {
			t.Fatalf("wrapper %s lacks host adapter", host)
		}
		for action := range assets.Playbooks {
			playbook, readErr := os.ReadFile(filepath.Join(dest, "workrun", "references", action+".md"))
			if readErr != nil || len(playbook) == 0 {
				t.Fatalf("playbook %s: %v", action, readErr)
			}
		}
	}
}
func TestDiagnosticExportRequiresManifestApprovalAndRedactsEvents(t *testing.T) {
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
	run, _, err := st.CreateOrResumeRun(ctx, store.CreateRunInput{Provider: "tracker", WorkItemID: "EXP-1", WorkItemKey: "EXP-1", Repository: t.TempDir(), WorkflowName: def.Name, WorkflowHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	run.WaitReason = "authorization: Bearer hidden-value"
	if err = st.UpdateRun(ctx, run, "contains_secret", "", map[string]string{"token": "hidden-value"}); err != nil {
		t.Fatal(err)
	}
	a := &app{ctx: ctx, store: st, workflow: def, workflowHash: hash}
	out := filepath.Join(t.TempDir(), "diagnostics.json")
	manifest := map[string]any{"run_id": run.ID, "output": out, "contents": []string{"run snapshot without source content", "event metadata without payloads", "database/protocol versions"}, "redacted": true}
	approval, err := engine.CanonicalHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.diagnostics([]string{"--out", out, "--approve", approval[:12], run.ID}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || contains(string(raw), "hidden-value") {
		t.Fatalf("diagnostics not redacted: %s", raw)
	}
	var bundle struct {
		State struct {
			ProtocolVersion string `json:"protocol_version"`
		} `json:"state"`
	}
	if err = json.Unmarshal(raw, &bundle); err != nil || bundle.State.ProtocolVersion != domain.ProtocolVersion {
		t.Fatalf("invalid bundle: %v %#v", err, bundle)
	}
}
func TestApprovedProposalStartsNormalChangeRun(t *testing.T) {
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
	repo := t.TempDir()
	cfg := config.Repository{SchemaVersion: 1, Protocol: ">=1.0 <2.0", TargetBranch: "develop", BranchTemplate: "task/{key}-{slug}", Requirements: config.RequirementPolicy{RequiredRoles: []string{"business", "functional"}, Traversal: []config.TraversalStep{{Kind: "description"}}, MaxDepth: 1}, TrackerIntents: map[string]string{"mark_in_review": "review", "mark_resolved": "resolved"}, Verification: config.VerificationPolicy{Policy: "discover"}, BaseUpdate: config.BaseUpdatePolicy{Strategy: "repository"}, Safety: config.SafetyPolicy{RequireBriefApproval: true, RequireReviewApproval: true}}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo, ".agent-workflow.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p := domain.Proposal{RunID: "source", Scope: "repository", Reason: "observed correction", ArtifactHash: "abcdef1234567890", Diff: json.RawMessage(`{"observed_correction":"correction","compatibility_impact":"compatible","tests_required":["go test ./..."],"diff":{"target_branch":"main"}}`)}
	if err = st.CreateProposal(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if err = st.ApproveProposal(ctx, p.ID, p.ArtifactHash); err != nil {
		t.Fatal(err)
	}
	a := &app{ctx: ctx, store: st, workflow: def, workflowHash: hash}
	if err = a.proposal([]string{"start-change", "--repo", repo, p.ID}); err != nil {
		t.Fatal(err)
	}
	runs, err := st.ListRuns(ctx, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].WorkItemProvider != "workrun-evolution" || runs[0].WorkItemID != p.ID {
		t.Fatalf("unexpected evolution run: %#v", runs)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
