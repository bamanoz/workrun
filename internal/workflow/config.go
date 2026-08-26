package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const maxWorkflowBytes = 1 << 20

var safeName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var safeCapability = regexp.MustCompile(`^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)+$`)
var allowedWaits = map[string]bool{"blocked": true, "paused": true}
var allowedRetryClasses = map[string]bool{"read_transient_safe": true, "write_reconcile_first": true}
var allowedOutcomes = map[string]map[string]bool{
	"reconcile": {"complete": true, "changed": true, "blocked": true}, "inspect_context": {"complete": true, "needs_input": true}, "prepare_workspace": {"complete": true, "blocked": true}, "implement_change": {"complete": true, "scope_changed": true, "blocked": true}, "verify_change": {"passed": true, "failed_fixable": true, "blocked": true}, "finalize_change": {"complete": true, "needs_changes": true, "blocked": true}, "publish_change": {"complete": true, "blocked": true}, "poll_change_request": {"no_change": true, "feedback": true, "feedback_independent": true, "feedback_overlap": true, "merged": true, "closed": true}, "analyze_review": {"complete": true, "no_action": true, "blocked": true}, "address_review": {"complete": true, "scope_changed": true, "blocked": true}, "resolve_work_item": {"complete": true, "blocked": true},
}

type Definition struct {
	SchemaVersion int                    `yaml:"schema_version" json:"schema_version"`
	Protocol      string                 `yaml:"protocol" json:"protocol"`
	Name          string                 `yaml:"name" json:"name"`
	Initial       string                 `yaml:"initial" json:"initial"`
	States        map[string]State       `yaml:"states" json:"states"`
	RetryPolicies map[string]RetryPolicy `yaml:"retry_policies,omitempty" json:"retry_policies,omitempty"`
	WakePolicies  map[string]WakePolicy  `yaml:"wake_policies,omitempty" json:"wake_policies,omitempty"`
}

type State struct {
	Action       string            `yaml:"action,omitempty" json:"action,omitempty"`
	Gate         string            `yaml:"gate,omitempty" json:"gate,omitempty"`
	Wait         string            `yaml:"wait,omitempty" json:"wait,omitempty"`
	RequiredCaps []string          `yaml:"required_capabilities,omitempty" json:"required_capabilities,omitempty"`
	Fallbacks    []Fallback        `yaml:"fallbacks,omitempty" json:"fallbacks,omitempty"`
	RetryPolicy  string            `yaml:"retry_policy,omitempty" json:"retry_policy,omitempty"`
	WakePolicy   string            `yaml:"wake_policy,omitempty" json:"wake_policy,omitempty"`
	On           map[string]string `yaml:"on,omitempty" json:"on,omitempty"`
	Terminal     bool              `yaml:"terminal,omitempty" json:"terminal,omitempty"`
}

type Fallback struct {
	WhenMissing string `yaml:"when_missing" json:"when_missing"`
	Use         string `yaml:"use" json:"use"`
}

type RetryPolicy struct {
	Class       string `yaml:"class" json:"class"`
	MaxAttempts int    `yaml:"max_attempts" json:"max_attempts"`
	Initial     string `yaml:"initial" json:"initial"`
	Maximum     string `yaml:"maximum" json:"maximum"`
}

type WakePolicy struct {
	Intervals []string `yaml:"intervals" json:"intervals"`
	WorkHours bool     `yaml:"work_hours" json:"work_hours"`
}

var allowedActions = map[string]struct{}{
	"inspect_context": {}, "prepare_workspace": {}, "implement_change": {},
	"verify_change": {}, "finalize_change": {}, "publish_change": {},
	"poll_change_request": {}, "analyze_review": {}, "address_review": {},
	"resolve_work_item": {}, "reconcile": {},
}

var allowedGates = map[string]struct{}{
	"approve_brief": {}, "approve_review_plan": {}, "approve_terminal": {},
	"approve_architecture": {}, "approve_migration": {},
}

func LoadFile(path string) (*Definition, []byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Size() > maxWorkflowBytes {
		return nil, nil, fmt.Errorf("workflow exceeds %d bytes", maxWorkflowBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	def, err := Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("parse workflow %s: %w", path, err)
	}
	return def, data, nil
}

func Parse(data []byte) (*Definition, error) {
	if len(data) > maxWorkflowBytes {
		return nil, fmt.Errorf("workflow exceeds %d bytes", maxWorkflowBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var def Definition
	if err := dec.Decode(&def); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("workflow must contain exactly one YAML document")
		}
		return nil, err
	}
	if err := def.Validate(); err != nil {
		return nil, err
	}
	return &def, nil
}

func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (d *Definition) Validate() error {
	if d.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d", d.SchemaVersion)
	}
	if !safeName.MatchString(d.Name) {
		return errors.New("name must be a safe workflow identifier")
	}
	if strings.TrimSpace(d.Protocol) != "\u003e=1.0 \u003c2.0" {
		return fmt.Errorf("protocol %q must equal the supported 1.x range", d.Protocol)
	}
	if len(d.States) == 0 {
		return errors.New("at least one state is required")
	}
	if _, ok := d.States[d.Initial]; !ok {
		return fmt.Errorf("initial state %q is not declared", d.Initial)
	}
	for name, policy := range d.RetryPolicies {
		if !safeName.MatchString(name) || !allowedRetryClasses[policy.Class] || policy.MaxAttempts < 1 || policy.MaxAttempts > 20 {
			return fmt.Errorf("invalid retry policy %q", name)
		}
		initial, err := time.ParseDuration(policy.Initial)
		if err != nil || initial <= 0 {
			return fmt.Errorf("retry policy %q has invalid initial duration", name)
		}
		maximum, err := time.ParseDuration(policy.Maximum)
		if err != nil || maximum < initial || maximum > 24*time.Hour {
			return fmt.Errorf("retry policy %q has invalid maximum duration", name)
		}
	}
	for name, policy := range d.WakePolicies {
		if !safeName.MatchString(name) || len(policy.Intervals) == 0 || len(policy.Intervals) > 16 {
			return fmt.Errorf("invalid wake policy %q", name)
		}
		var prior time.Duration
		for _, raw := range policy.Intervals {
			interval, err := time.ParseDuration(raw)
			if err != nil || interval <= prior || interval > 24*time.Hour {
				return fmt.Errorf("wake policy %q intervals must be positive, increasing, and at most 24h", name)
			}
			prior = interval
		}
	}
	names := make([]string, 0, len(d.States))
	for name := range d.States {
		names = append(names, name)
	}
	sort.Strings(names)
	gates := map[string]int{}
	for _, name := range names {
		if !safeName.MatchString(name) {
			return fmt.Errorf("invalid state name %q", name)
		}
		state := d.States[name]
		modes := 0
		if state.Action != "" {
			modes++
		}
		if state.Gate != "" {
			modes++
		}
		if state.Wait != "" {
			modes++
		}
		if state.Terminal {
			modes++
		}
		if modes != 1 {
			return fmt.Errorf("state %q must declare exactly one of action, gate, wait, or terminal", name)
		}
		if state.Action != "" {
			if _, ok := allowedActions[state.Action]; !ok {
				return fmt.Errorf("state %q uses unknown action %q", name, state.Action)
			}
			if len(state.On) == 0 {
				return fmt.Errorf("action state %q needs transitions", name)
			}
			for outcome := range state.On {
				if !allowedOutcomes[state.Action][outcome] {
					return fmt.Errorf("action %q does not support outcome %q", state.Action, outcome)
				}
			}
		}
		if state.Gate != "" {
			if _, ok := allowedGates[state.Gate]; !ok {
				return fmt.Errorf("state %q uses unknown gate %q", name, state.Gate)
			}
			gates[state.Gate]++
			if len(state.On) == 0 {
				return fmt.Errorf("gate state %q needs transitions", name)
			}
		}
		if state.Wait != "" {
			if !allowedWaits[state.Wait] {
				return fmt.Errorf("state %q uses unknown wait %q", name, state.Wait)
			}
			if len(state.On) == 0 {
				return fmt.Errorf("wait state %q needs transitions", name)
			}
		}
		if state.Terminal && len(state.On) != 0 {
			return fmt.Errorf("terminal state %q cannot have transitions", name)
		}
		seenCaps := map[string]bool{}
		for _, capability := range state.RequiredCaps {
			if !safeCapability.MatchString(capability) || seenCaps[capability] {
				return fmt.Errorf("state %q has invalid or duplicate capability %q", name, capability)
			}
			seenCaps[capability] = true
		}
		seenFallbacks := map[string]bool{}
		for _, fallback := range state.Fallbacks {
			if !safeCapability.MatchString(fallback.WhenMissing) || !safeCapability.MatchString(fallback.Use) || fallback.WhenMissing == fallback.Use || seenFallbacks[fallback.WhenMissing] {
				return fmt.Errorf("state %q has invalid fallback", name)
			}
			seenFallbacks[fallback.WhenMissing] = true
		}
		if state.Action == "" && (len(state.RequiredCaps) > 0 || len(state.Fallbacks) > 0 || state.RetryPolicy != "" || state.WakePolicy != "") {
			return fmt.Errorf("non-action state %q cannot declare execution policy", name)
		}
		if state.RetryPolicy != "" {
			if _, ok := d.RetryPolicies[state.RetryPolicy]; !ok {
				return fmt.Errorf("state %q references unknown retry policy %q", name, state.RetryPolicy)
			}
		}
		if state.WakePolicy != "" {
			if _, ok := d.WakePolicies[state.WakePolicy]; !ok {
				return fmt.Errorf("state %q references unknown wake policy %q", name, state.WakePolicy)
			}
		}
		for outcome, target := range state.On {
			if !safeName.MatchString(outcome) {
				return fmt.Errorf("state %q has invalid outcome %q", name, outcome)
			}
			if _, ok := d.States[target]; !ok {
				return fmt.Errorf("state %q outcome %q targets unknown state %q", name, outcome, target)
			}
		}
	}
	if gates["approve_brief"] != 1 || gates["approve_review_plan"] != 1 {
		return errors.New("workflow requires exactly one brief and review-plan approval gate")
	}
	if resolved, ok := d.States["resolved"]; !ok || !resolved.Terminal {
		return errors.New("workflow requires terminal resolved state")
	}
	return d.validateSafetyFlow()
}

func (d *Definition) validateSafetyFlow() error {
	statesForAction := func(action string) []string {
		var out []string
		for name, state := range d.States {
			if state.Action == action {
				out = append(out, name)
			}
		}
		sort.Strings(out)
		return out
	}
	gateState := func(gate string) (string, bool) {
		for name, state := range d.States {
			if state.Gate == gate {
				return name, true
			}
		}
		return "", false
	}
	requireTarget := func(from, outcome string, predicate func(State) bool) error {
		state, ok := d.States[from]
		if !ok {
			return fmt.Errorf("required lifecycle state %q is missing", from)
		}
		target, ok := state.On[outcome]
		if !ok || !predicate(d.States[target]) {
			return fmt.Errorf("state %q outcome %q violates the safety lifecycle", from, outcome)
		}
		return nil
	}
	unique := func(action string) (string, error) {
		states := statesForAction(action)
		if len(states) != 1 {
			return "", fmt.Errorf("workflow requires exactly one %s action state", action)
		}
		return states[0], nil
	}
	inspectStates := statesForAction("inspect_context")
	if len(inspectStates) == 0 {
		return errors.New("workflow requires context inspection")
	}
	briefGate, _ := gateState("approve_brief")
	for _, inspect := range inspectStates {
		if d.States[inspect].On["complete"] != briefGate {
			return errors.New("every context inspection must enter brief approval")
		}
	}
	prepare, err := unique("prepare_workspace")
	if err != nil {
		return err
	}
	if err = requireTarget(briefGate, "approved", func(s State) bool { return s.Action == "prepare_workspace" }); err != nil {
		return err
	}
	implement, err := unique("implement_change")
	if err != nil {
		return err
	}
	if err = requireTarget(prepare, "complete", func(s State) bool { return s.Action == "implement_change" }); err != nil {
		return err
	}
	verify, err := unique("verify_change")
	if err != nil {
		return err
	}
	if err = requireTarget(implement, "complete", func(s State) bool { return s.Action == "verify_change" }); err != nil {
		return err
	}
	finalize, err := unique("finalize_change")
	if err != nil {
		return err
	}
	if err = requireTarget(verify, "passed", func(s State) bool { return s.Action == "finalize_change" }); err != nil {
		return err
	}
	publish, err := unique("publish_change")
	if err != nil {
		return err
	}
	if err = requireTarget(finalize, "complete", func(s State) bool { return s.Action == "publish_change" }); err != nil {
		return err
	}
	if err = requireTarget(publish, "complete", func(s State) bool { return s.Action == "poll_change_request" }); err != nil {
		return err
	}
	analyze, err := unique("analyze_review")
	if err != nil {
		return err
	}
	reviewGate, _ := gateState("approve_review_plan")
	if err = requireTarget(analyze, "complete", func(s State) bool { return s.Gate == "approve_review_plan" }); err != nil {
		return err
	}
	if err = requireTarget(reviewGate, "approved", func(s State) bool { return s.Action == "poll_change_request" }); err != nil {
		return err
	}
	address, err := unique("address_review")
	if err != nil {
		return err
	}
	if err = requireTarget(address, "complete", func(s State) bool { return s.Action == "poll_change_request" }); err != nil {
		return err
	}
	resolve, err := unique("resolve_work_item")
	if err != nil {
		return err
	}
	if d.States[resolve].On["complete"] != "resolved" {
		return errors.New("resolution action must enter resolved")
	}
	for _, poll := range statesForAction("poll_change_request") {
		if err = requireTarget(poll, "merged", func(s State) bool { return s.Action == "resolve_work_item" }); err != nil {
			return err
		}
	}
	return nil
}
