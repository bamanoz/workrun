package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bamanoz/workrun/internal/pathsecurity"
	"go.yaml.in/yaml/v3"
)

const maxConfigurationBytes = 1 << 20

var safeIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var safeCapability = regexp.MustCompile(`^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)+$`)
var safeRole = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var sha256Hash = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Repository struct {
	SchemaVersion  int                `yaml:"schema_version" json:"schema_version"`
	Protocol       string             `yaml:"protocol" json:"protocol"`
	Language       string             `yaml:"language,omitempty" json:"language,omitempty"`
	TargetBranch   string             `yaml:"target_branch" json:"target_branch"`
	BranchTemplate string             `yaml:"branch_template" json:"branch_template"`
	Requirements   RequirementPolicy  `yaml:"requirements" json:"requirements"`
	TrackerIntents map[string]string  `yaml:"tracker_intents" json:"tracker_intents"`
	Verification   VerificationPolicy `yaml:"verification" json:"verification"`
	BaseUpdate     BaseUpdatePolicy   `yaml:"base_update" json:"base_update"`
	Safety         SafetyPolicy       `yaml:"safety,omitempty" json:"safety,omitempty"`
}

type RequirementPolicy struct {
	RequiredRoles  []string        `yaml:"required_roles" json:"required_roles"`
	Traversal      []TraversalStep `yaml:"traversal" json:"traversal"`
	MaxDepth       int             `yaml:"max_depth" json:"max_depth"`
	CandidateLimit int             `yaml:"candidate_limit,omitempty" json:"candidate_limit,omitempty"`
}

type TraversalStep struct {
	Kind          string   `yaml:"kind" json:"kind"`
	Relationships []string `yaml:"relationships,omitempty" json:"relationships,omitempty"`
}

type VerificationPolicy struct {
	Policy string `yaml:"policy" json:"policy"`
}
type BaseUpdatePolicy struct {
	Strategy string `yaml:"strategy" json:"strategy"`
}
type SafetyPolicy struct {
	RequireBriefApproval  bool `yaml:"require_brief_approval,omitempty" json:"require_brief_approval"`
	RequireReviewApproval bool `yaml:"require_review_approval,omitempty" json:"require_review_approval"`
	AllowForcePush        bool `yaml:"allow_force_push,omitempty" json:"allow_force_push"`
	AllowAutomaticMerge   bool `yaml:"allow_automatic_merge,omitempty" json:"allow_automatic_merge"`
}

type User struct {
	SchemaVersion int                `yaml:"schema_version" json:"schema_version"`
	Protocol      string             `yaml:"protocol" json:"protocol"`
	Timezone      string             `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	WorkHours     WorkHours          `yaml:"work_hours,omitempty" json:"work_hours,omitempty"`
	Bindings      map[string]Binding `yaml:"bindings" json:"bindings"`
	Trust         TrustPolicy        `yaml:"trust" json:"trust"`
	RetentionDays int                `yaml:"retention_days,omitempty" json:"retention_days,omitempty"`
}

type TrustPolicy struct {
	AllowedProviders []string `yaml:"allowed_providers" json:"allowed_providers"`
	AllowedURLHosts  []string `yaml:"allowed_url_hosts" json:"allowed_url_hosts"`
	MaxContentBytes  int      `yaml:"max_content_bytes" json:"max_content_bytes"`
}

type WorkHours struct {
	Start string `yaml:"start,omitempty" json:"start,omitempty"`
	End   string `yaml:"end,omitempty" json:"end,omitempty"`
}
type Binding struct {
	Server     string            `yaml:"server" json:"server"`
	Tool       string            `yaml:"tool" json:"tool"`
	SchemaHash string            `yaml:"schema_hash" json:"schema_hash"`
	Fields     map[string]string `yaml:"fields,omitempty" json:"fields,omitempty"`
}

func LoadRepository(path string) (*Repository, []byte, error) {
	var cfg Repository
	raw, err := loadStrict(path, &cfg)
	if err != nil {
		return nil, nil, err
	}
	if err = cfg.Validate(); err != nil {
		return nil, nil, err
	}
	return &cfg, raw, nil
}

func ParseRepository(raw []byte) (*Repository, error) {
	var cfg Repository
	if err := decodeStrict(raw, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func LoadUser(path string) (*User, []byte, error) {
	if _, err := os.Lstat(path); err != nil {
		return nil, nil, err
	}
	if err := pathsecurity.ValidateFile(path); err != nil {
		return nil, nil, fmt.Errorf("unsafe user configuration: %w", err)
	}
	if err := pathsecurity.ValidateDir(filepath.Dir(path), true); err != nil {
		return nil, nil, fmt.Errorf("unsafe user configuration directory: %w", err)
	}
	var cfg User
	raw, err := loadStrict(path, &cfg)
	if err != nil {
		return nil, nil, err
	}
	if err = cfg.Validate(); err != nil {
		return nil, nil, err
	}
	return &cfg, raw, nil
}

func ParseUser(raw []byte) (*User, error) {
	var cfg User
	if err := decodeStrict(raw, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func loadStrict(path string, target any) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxConfigurationBytes {
		return nil, fmt.Errorf("configuration exceeds %d bytes", maxConfigurationBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err = decodeStrict(raw, target); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return raw, nil
}

func decodeStrict(raw []byte, target any) error {
	if len(raw) > maxConfigurationBytes {
		return fmt.Errorf("configuration exceeds %d bytes", maxConfigurationBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("configuration must contain one YAML document")
		}
		return err
	}
	return nil
}

func Hash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func (r *Repository) Validate() error {
	if r.SchemaVersion != 1 {
		return fmt.Errorf("unsupported repository schema_version %d", r.SchemaVersion)
	}
	if strings.TrimSpace(r.Protocol) != ">=1.0 <2.0" {
		return errors.New("repository protocol must be >=1.0 <2.0")
	}
	if strings.TrimSpace(r.TargetBranch) != r.TargetBranch || !validGitRef(r.TargetBranch) {
		return errors.New("target_branch must be a safe Git branch name")
	}
	if !validBranchTemplate(r.BranchTemplate) {
		return errors.New("branch_template must be a safe template containing {key}")
	}
	if len(r.Requirements.RequiredRoles) == 0 {
		return errors.New("requirements.required_roles is required")
	}
	roles := map[string]bool{}
	for _, role := range r.Requirements.RequiredRoles {
		if !safeRole.MatchString(role) || roles[role] {
			return errors.New("requirements.required_roles must contain unique safe role names")
		}
		roles[role] = true
	}
	if r.Requirements.MaxDepth < 0 || r.Requirements.MaxDepth > 5 {
		return errors.New("requirements.max_depth must be between 0 and 5")
	}
	if len(r.Requirements.Traversal) == 0 {
		return errors.New("requirements.traversal is required")
	}
	if r.Requirements.CandidateLimit == 0 {
		r.Requirements.CandidateLimit = 20
	}
	if r.Requirements.CandidateLimit < 1 || r.Requirements.CandidateLimit > 100 {
		return errors.New("requirements.candidate_limit must be between 1 and 100")
	}
	steps := map[string]bool{}
	for _, step := range r.Requirements.Traversal {
		if steps[step.Kind] {
			return fmt.Errorf("duplicate requirements traversal kind %q", step.Kind)
		}
		steps[step.Kind] = true
		switch step.Kind {
		case "description", "parent":
			if len(step.Relationships) > 0 {
				return fmt.Errorf("requirements traversal %s cannot declare relationships", step.Kind)
			}
		case "relationships":
			if len(step.Relationships) == 0 {
				return errors.New("relationships traversal requires an allowlist")
			}
			seen := map[string]bool{}
			for _, relationship := range step.Relationships {
				if !safeRole.MatchString(relationship) || seen[relationship] {
					return errors.New("relationship allowlist must contain unique safe names")
				}
				seen[relationship] = true
			}
		default:
			return fmt.Errorf("unsupported requirements traversal kind %q", step.Kind)
		}
	}
	for _, intent := range []string{"mark_in_review", "mark_resolved"} {
		if !safeIdentifier.MatchString(r.TrackerIntents[intent]) || r.TrackerIntents[intent] == "configure-me" {
			return fmt.Errorf("tracker_intents.%s must be a safe configured identifier", intent)
		}
	}
	if r.Safety.AllowAutomaticMerge {
		return errors.New("safety lattice forbids automatic merge")
	}
	if r.Safety.AllowForcePush {
		return errors.New("safety lattice forbids a standing force-push override")
	}
	if !r.Safety.RequireBriefApproval || !r.Safety.RequireReviewApproval {
		return errors.New("safety requires brief and review-plan approvals")
	}
	if !safeIdentifier.MatchString(r.Verification.Policy) || !safeIdentifier.MatchString(r.BaseUpdate.Strategy) {
		return errors.New("verification and base-update policies must be safe identifiers")
	}
	if r.Language != "" && !validLanguage(r.Language) {
		return errors.New("language must be a BCP-47 style tag")
	}
	return nil
}

func (u *User) Validate() error {
	if u.SchemaVersion != 1 {
		return fmt.Errorf("unsupported user schema_version %d", u.SchemaVersion)
	}
	if strings.TrimSpace(u.Protocol) != ">=1.0 <2.0" {
		return errors.New("user protocol must be >=1.0 <2.0")
	}
	if len(u.Trust.AllowedProviders) == 0 || len(u.Trust.AllowedURLHosts) == 0 {
		return errors.New("trust.allowed_providers and trust.allowed_url_hosts are required")
	}
	if u.Trust.MaxContentBytes == 0 {
		u.Trust.MaxContentBytes = 1 << 20
	}
	if u.Trust.MaxContentBytes < 1024 || u.Trust.MaxContentBytes > 8<<20 {
		return errors.New("trust.max_content_bytes must be between 1 KiB and 8 MiB")
	}
	if u.RetentionDays == 0 {
		u.RetentionDays = 30
	}
	if u.RetentionDays < 1 {
		return errors.New("retention_days must be positive")
	}
	if u.Timezone != "" && u.Timezone != "Local" {
		if _, err := time.LoadLocation(u.Timezone); err != nil {
			return fmt.Errorf("invalid timezone: %w", err)
		}
	}
	if err := validateWorkHours(u.WorkHours); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, provider := range u.Trust.AllowedProviders {
		if strings.TrimSpace(provider) != provider || !safeIdentifier.MatchString(provider) || seen[provider] {
			return errors.New("trust.allowed_providers must contain unique safe identifiers")
		}
		seen[provider] = true
	}
	seen = map[string]bool{}
	for _, host := range u.Trust.AllowedURLHosts {
		if strings.ToLower(strings.TrimSpace(host)) != host || strings.ContainsAny(host, "/:@") || !safeIdentifier.MatchString(host) || seen[host] {
			return fmt.Errorf("invalid or duplicate trusted host %q", host)
		}
		seen[host] = true
	}
	for capability, binding := range u.Bindings {
		if !safeCapability.MatchString(capability) || !safeIdentifier.MatchString(binding.Server) || !safeIdentifier.MatchString(binding.Tool) || !sha256Hash.MatchString(binding.SchemaHash) {
			return fmt.Errorf("binding %q requires safe capability/server/tool and a lowercase SHA-256 schema_hash", capability)
		}
	}
	return nil
}

func validGitRef(value string) bool {
	return value != "" && !strings.HasPrefix(value, "-") && !strings.HasPrefix(value, ".") && !strings.HasSuffix(value, ".") && !strings.HasSuffix(value, "/") && !strings.HasSuffix(value, ".lock") && !strings.Contains(value, "..") && !strings.Contains(value, "//") && !strings.Contains(value, "@{") && !strings.ContainsAny(value, " ~^:?*[\\")
}

func validBranchTemplate(value string) bool {
	if !strings.Contains(value, "{key}") {
		return false
	}
	stripped := strings.ReplaceAll(strings.ReplaceAll(value, "{key}", "key"), "{slug}", "slug")
	return !strings.ContainsAny(stripped, "{}") && validGitRef(stripped)
}

func validLanguage(value string) bool {
	for _, part := range strings.Split(value, "-") {
		if len(part) < 2 || len(part) > 8 {
			return false
		}
		for _, r := range part {
			if r < 'A' || r > 'Z' && r < 'a' || r > 'z' {
				return false
			}
		}
	}
	return true
}

func validateWorkHours(hours WorkHours) error {
	if hours.Start == "" && hours.End == "" {
		return nil
	}
	start, err := clockMinutes(hours.Start)
	if err != nil {
		return fmt.Errorf("invalid work_hours.start: %w", err)
	}
	end, err := clockMinutes(hours.End)
	if err != nil {
		return fmt.Errorf("invalid work_hours.end: %w", err)
	}
	if start >= end {
		return errors.New("work_hours.start must be before work_hours.end")
	}
	return nil
}

func clockMinutes(value string) (int, error) {
	if len(value) != 5 || value[2] != ':' {
		return 0, errors.New("expected HH:MM")
	}
	hour, err := strconv.Atoi(value[:2])
	if err != nil {
		return 0, err
	}
	minute, err := strconv.Atoi(value[3:])
	if err != nil {
		return 0, err
	}
	if hour > 23 || minute > 59 {
		return 0, errors.New("clock value out of range")
	}
	return hour*60 + minute, nil
}

func (u *User) AllowsProvider(provider string) bool {
	for _, allowed := range u.Trust.AllowedProviders {
		if allowed == provider {
			return true
		}
	}
	return false
}

func (u *User) AllowsURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || parsed.Port() != "" && parsed.Port() != "443" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, allowed := range u.Trust.AllowedURLHosts {
		if host == allowed {
			return true
		}
	}
	return false
}

func FindRepository(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, ".agent-workflow.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", os.ErrNotExist
}
func DefaultUserPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "workrun", "config.yaml"), nil
}

func UserSecurityInfo(path string) (pathsecurity.Info, pathsecurity.Info) {
	return pathsecurity.Inspect(path, false, true), pathsecurity.Inspect(filepath.Dir(path), true, true)
}
