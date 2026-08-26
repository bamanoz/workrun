package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryManifestValidation(t *testing.T) {
	cfg := Repository{SchemaVersion: 1, Protocol: ">=1.0 <2.0", TargetBranch: "develop", BranchTemplate: "task/{key}-{slug}", Requirements: RequirementPolicy{RequiredRoles: []string{"business", "functional"}, Traversal: []TraversalStep{{Kind: "description"}}, MaxDepth: 1}, TrackerIntents: map[string]string{"mark_in_review": "transition-4", "mark_resolved": "transition-7"}, Verification: VerificationPolicy{Policy: "discover"}, BaseUpdate: BaseUpdatePolicy{Strategy: "repository"}, Safety: SafetyPolicy{RequireBriefApproval: true, RequireReviewApproval: true}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Safety.AllowAutomaticMerge = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("automatic merge must be rejected by safety lattice")
	}
}

func TestRepositoryManifestUsesStrictYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".agent-workflow.yaml")
	raw := []byte("schema_version: 1\nprotocol: '>=1.0 <2.0'\nunknown: true\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadRepository(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestBindingRequiresSchemaHash(t *testing.T) {
	cfg := User{SchemaVersion: 1, Protocol: ">=1.0 <2.0", Bindings: map[string]Binding{"tracker.read": {Server: "tracker", Tool: "read"}}, Trust: TrustPolicy{AllowedProviders: []string{"tracker"}, AllowedURLHosts: []string{"tracker.local"}, MaxContentBytes: 1024}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("binding without schema hash accepted")
	}
}

func TestTrustPolicyAllowsOnlyPinnedHTTPSHosts(t *testing.T) {
	cfg := User{SchemaVersion: 1, Protocol: ">=1.0 <2.0", Bindings: map[string]Binding{}, Trust: TrustPolicy{AllowedProviders: []string{"tracker"}, AllowedURLHosts: []string{"tracker.local"}, MaxContentBytes: 1024}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowsProvider("tracker") || cfg.AllowsProvider("other") {
		t.Fatal("provider allowlist ignored")
	}
	if !cfg.AllowsURL("https://tracker.local/item/1") || cfg.AllowsURL("http://tracker.local/item/1") || cfg.AllowsURL("https://evil.local/item/1") {
		t.Fatal("URL trust policy ignored")
	}
}
