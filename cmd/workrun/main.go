package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bamanoz/workrun/internal/assets"
	"github.com/bamanoz/workrun/internal/config"
	"github.com/bamanoz/workrun/internal/domain"
	"github.com/bamanoz/workrun/internal/engine"
	"github.com/bamanoz/workrun/internal/store"
	"github.com/bamanoz/workrun/internal/workflow"
	"go.yaml.in/yaml/v3"
)

var version = "0.1.0"

type app struct {
	ctx          context.Context
	store        *store.Store
	workflow     *workflow.Definition
	workflowHash string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "workrun:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	if args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	if args[0] == "init-user" {
		return initUser(args[1:])
	}
	if args[0] == "install-agent" {
		return installAgent(args[1:])
	}
	if args[0] == "init-repo" {
		return initRepository(args[1:])
	}
	ctx := context.Background()
	dbPath := os.Getenv("WORKRUN_DB")
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	retentionDays := 30
	if path, pathErr := config.DefaultUserPath(); pathErr == nil {
		if user, _, loadErr := config.LoadUser(path); loadErr == nil {
			retentionDays = user.RetentionDays
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}
	}
	if _, err := st.PurgeExpiredPayloads(ctx, time.Duration(retentionDays)*24*time.Hour); err != nil {
		return fmt.Errorf("retention maintenance: %w", err)
	}
	def, err := workflow.Parse(assets.TrackedChangeWorkflow)
	if err != nil {
		return fmt.Errorf("embedded workflow: %w", err)
	}
	hash := workflow.Hash(assets.TrackedChangeWorkflow)
	if err := st.SaveWorkflow(ctx, hash, def.Name, def.Protocol, assets.TrackedChangeWorkflow); err != nil {
		return err
	}
	a := &app{ctx: ctx, store: st, workflow: def, workflowHash: hash}
	switch args[0] {
	case "start":
		return a.start(args[1:])
	case "status":
		return a.status(args[1:])
	case "resume":
		return a.resume(args[1:])
	case "approve":
		return a.approve(args[1:])
	case "pause":
		return a.pause(args[1:])
	case "cancel":
		return a.cancel(args[1:])
	case "fail-permanent":
		return a.failPermanent(args[1:])
	case "cleanup":
		return a.cleanup(args[1:])
	case "export":
		return a.exportState(args[1:])
	case "diagnostics":
		return a.diagnostics(args[1:])
	case "proposal":
		return a.proposal(args[1:])
	case "migrate":
		return a.migrate(args[1:])
	case "doctor":
		return a.doctor(args[1:])
	case "agent":
		return a.agent(args[1:])
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: workrun <start|status|resume|approve|pause|cancel|cleanup|proposal|migrate|doctor|export|diagnostics|init-user|init-repo|install-agent|agent|version>")
}

func (a *app) engineFor(runID string) (*engine.Engine, error) {
	run, err := a.store.GetRun(a.ctx, runID)
	if err != nil {
		return nil, err
	}
	def := a.workflow
	if run.WorkflowHash != a.workflowHash {
		raw, err := a.store.GetWorkflow(a.ctx, run.WorkflowHash)
		if err != nil {
			return nil, err
		}
		def, err = workflow.Parse(raw)
		if err != nil {
			return nil, err
		}
	}
	eng := engine.New(a.store, def)
	path, err := config.DefaultUserPath()
	if err != nil {
		return nil, err
	}
	user, _, err := config.LoadUser(path)
	if err == nil {
		eng.User = user
		zone := time.Local
		if user.Timezone != "" {
			zone, err = time.LoadLocation(user.Timezone)
			if err != nil {
				return nil, fmt.Errorf("load timezone: %w", err)
			}
		}
		startH, startM, endH, endM := 9, 0, 18, 0
		if user.WorkHours.Start != "" {
			if _, err = fmt.Sscanf(user.WorkHours.Start, "%d:%d", &startH, &startM); err != nil {
				return nil, fmt.Errorf("parse work_hours.start: %w", err)
			}
		}
		if user.WorkHours.End != "" {
			if _, err = fmt.Sscanf(user.WorkHours.End, "%d:%d", &endH, &endM); err != nil {
				return nil, fmt.Errorf("parse work_hours.end: %w", err)
			}
		}
		if err = eng.ConfigureWorkHours(zone, startH, startM, endH, endM); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return eng, nil
}

func (a *app) start(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	provider := fs.String("provider", "tracker", "normalized tracker provider")
	repo := fs.String("repo", ".", "repository path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("start requires one work-item reference")
	}
	repoAbs, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	manifestPath, err := config.FindRepository(repoAbs)
	if err != nil {
		return fmt.Errorf("repository manifest not found; run workrun init-repo in %s", repoAbs)
	}
	_, raw, err := config.LoadRepository(manifestPath)
	if err != nil {
		return err
	}
	manifestHash := config.Hash(raw)
	if err := a.store.SaveManifest(a.ctx, manifestHash, raw); err != nil {
		return err
	}
	userPath, err := config.DefaultUserPath()
	if err != nil {
		return err
	}
	user, _, err := config.LoadUser(userPath)
	if err != nil {
		return fmt.Errorf("user MCP binding config is required; run workrun init-user: %w", err)
	}
	if !user.AllowsProvider(*provider) {
		return fmt.Errorf("provider %q is not allowed by user trust policy", *provider)
	}
	for _, cap := range []string{"tracker.read", "requirements.read", "tracker.transition", "tracker.comment", "change_request.open", "change_request.poll", "change_request.reply"} {
		if _, ok := user.Bindings[cap]; !ok {
			return fmt.Errorf("user binding %s is required", cap)
		}
	}
	ref := fs.Arg(0)
	run, created, err := a.store.CreateOrResumeRun(a.ctx, store.CreateRunInput{Provider: *provider, WorkItemID: ref, WorkItemKey: ref, Repository: repoAbs, WorkflowName: a.workflow.Name, WorkflowHash: a.workflowHash, ManifestHash: manifestHash})
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"created": created, "run": run})
}

func (a *app) status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	history := fs.Bool("history", false, "include event history")
	all := fs.Bool("all", false, "include terminal runs")
	due := fs.Bool("due", false, "show due runs only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("status accepts at most one run ID")
	}
	if fs.NArg() == 1 {
		run, err := a.store.GetRun(a.ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		if *history {
			events, err := a.store.Events(a.ctx, run.ID)
			if err != nil {
				return err
			}
			for i := range events {
				events[i].Data = engine.HistoryEventData(events[i].Data)
			}
			return printJSON(map[string]any{"run": run, "events": events})
		}
		return printJSON(run)
	}
	runs, err := a.store.ListRuns(a.ctx, !*all, *due)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(runs)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "RUN\tWORK ITEM\tREPOSITORY\tSTATE\tBRANCH\tCHANGE REQUEST\tWAIT / NEXT WAKE\tLEASE")
	for _, r := range runs {
		cr := ""
		if r.ChangeRequest != nil {
			cr = r.ChangeRequest.URL
		}
		wait := r.WaitReason
		if r.NextWakeAt != nil {
			wait = wait + " / " + r.NextWakeAt.Format(time.RFC3339)
		}
		owner, expires, _ := a.store.LeaseSummary(a.ctx, r.ID)
		lease := ""
		if expires != nil {
			lease = owner + " until " + expires.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.WorkItemKey, r.Repository, r.State, r.Branch, cr, wait, lease)
	}
	return w.Flush()
}

func (a *app) resume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	due := fs.Bool("due", false, "list due active runs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *due {
		runs, err := a.store.ListRuns(a.ctx, true, true)
		if err != nil {
			return err
		}
		return printJSON(runs)
	}
	if fs.NArg() != 1 {
		return errors.New("resume requires a run ID or --due")
	}
	eng, err := a.engineFor(fs.Arg(0))
	if err != nil {
		return err
	}
	run, err := eng.Resume(a.ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	return printJSON(run)
}

func (a *app) approve(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	actor := fs.String("by", os.Getenv("USER"), "approver identity")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("approve requires run ID and artifact hash prefix")
	}
	eng, err := a.engineFor(fs.Arg(0))
	if err != nil {
		return err
	}
	run, err := eng.Approve(a.ctx, fs.Arg(0), fs.Arg(1), *actor)
	if err != nil {
		return err
	}
	return printJSON(run)
}
func (a *app) pause(args []string) error {
	fs := flag.NewFlagSet("pause", flag.ContinueOnError)
	reason := fs.String("reason", "paused by user", "reason")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("pause requires run ID")
	}
	eng, err := a.engineFor(fs.Arg(0))
	if err != nil {
		return err
	}
	run, err := eng.Pause(a.ctx, fs.Arg(0), *reason)
	if err != nil {
		return err
	}
	return printJSON(run)
}
func (a *app) cancel(args []string) error { return a.terminalCommand(args, domain.StateCancelled) }
func (a *app) failPermanent(args []string) error {
	return a.terminalCommand(args, domain.StateFailedPermanent)
}
func (a *app) terminalCommand(args []string, target domain.State) error {
	fs := flag.NewFlagSet(string(target), flag.ContinueOnError)
	reason := fs.String("reason", "", "required reason")
	actor := fs.String("by", os.Getenv("USER"), "actor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%s requires run ID", target)
	}
	eng, err := a.engineFor(fs.Arg(0))
	if err != nil {
		return err
	}
	run, err := eng.Terminal(a.ctx, fs.Arg(0), target, *reason, *actor)
	if err != nil {
		return err
	}
	return printJSON(run)
}

func (a *app) cleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	approval := fs.String("approve", "", "approved cleanup hash prefix")
	evidenceText := fs.String("evidence", "", "JSON evidence from the host workspace tool")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("cleanup requires run ID")
	}
	run, err := a.store.GetRun(a.ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if !run.State.Terminal() {
		return errors.New("cleanup is allowed only for terminal runs")
	}
	if run.Workspace == "" {
		return printJSON(map[string]any{"run_id": run.ID, "clean": true})
	}
	artifact := map[string]string{"run_id": run.ID, "workspace": run.Workspace, "operation": "remove_local_workspace_only"}
	hash, err := engine.CanonicalHash(artifact)
	if err != nil {
		return err
	}
	if *approval == "" {
		return printJSON(map[string]any{"cleanup": artifact, "artifact_hash": hash, "approval_required": true, "remote_artifacts_preserved": true})
	}
	if len(strings.TrimSpace(*approval)) < 8 || !strings.HasPrefix(hash, strings.TrimSpace(*approval)) {
		return errors.New("approval hash does not match cleanup preview")
	}
	var evidence struct {
		WorkspaceRemoved bool `json:"workspace_removed"`
		Reconciled       bool `json:"reconciled"`
	}
	if err = decodeJSON([]byte(*evidenceText), &evidence); err != nil {
		return errors.New("approved cleanup requires exact workspace_removed and reconciled JSON evidence")
	}
	if !evidence.WorkspaceRemoved || !evidence.Reconciled {
		return errors.New("cleanup evidence requires workspace_removed=true and reconciled=true")
	}
	old := run.Workspace
	run.Workspace = ""
	if err := a.store.UpdateRun(a.ctx, run, "workspace_cleaned", "", map[string]any{"workspace": old, "workspace_removed": true, "reconciled": true}); err != nil {
		return err
	}
	return printJSON(run)
}

func (a *app) migrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	path := fs.String("workflow", "", "new workflow YAML")
	approval := fs.String("approve", "", "approved migration hash prefix")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *path == "" {
		return errors.New("migrate requires run ID and --workflow")
	}
	run, err := a.store.GetRun(a.ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	def, raw, err := workflow.LoadFile(*path)
	if err != nil {
		return err
	}
	newState, ok := def.States[string(run.State)]
	if !ok {
		return fmt.Errorf("new workflow does not contain current state %q", run.State)
	}
	if _, pendingErr := a.store.PendingAction(a.ctx, run.ID); pendingErr == nil {
		return errors.New("cannot migrate a run with a pending action")
	} else if !errors.Is(pendingErr, store.ErrNotFound) {
		return pendingErr
	}
	oldRaw, err := a.store.GetWorkflow(a.ctx, run.WorkflowHash)
	if err != nil {
		return err
	}
	oldDef, err := workflow.Parse(oldRaw)
	if err != nil {
		return err
	}
	oldState := oldDef.States[string(run.State)]
	newHash := workflow.Hash(raw)
	artifact := map[string]any{"run_id": run.ID, "from_workflow": run.WorkflowHash, "to_workflow": newHash, "current_state": run.State, "old_state": map[string]any{"action": oldState.Action, "gate": oldState.Gate, "wait": oldState.Wait, "outcomes": oldState.On}, "new_state": map[string]any{"action": newState.Action, "gate": newState.Gate, "wait": newState.Wait, "outcomes": newState.On}}
	migrationHash, err := engine.CanonicalHash(artifact)
	if err != nil {
		return err
	}
	preview := map[string]any{"migration": artifact, "artifact_hash": migrationHash, "approval_required": true}
	if *approval == "" {
		return printJSON(preview)
	}
	if len(strings.TrimSpace(*approval)) < 8 || !strings.HasPrefix(migrationHash, strings.TrimSpace(*approval)) {
		return errors.New("approval hash does not match migration preview")
	}
	if err := a.store.SaveWorkflow(a.ctx, newHash, def.Name, def.Protocol, raw); err != nil {
		return err
	}
	run.WorkflowHash = newHash
	run.WorkflowName = def.Name
	run.ProtocolVersion = domain.ProtocolVersion
	if err := a.store.UpdateRun(a.ctx, run, "workflow_migrated", "", artifact); err != nil {
		return err
	}
	return printJSON(run)
}

func (a *app) proposal(args []string) error {
	if len(args) == 0 {
		return errors.New("proposal requires list, show, create, approve, or start-change")
	}
	switch args[0] {
	case "list":
		items, err := a.store.ListProposals(a.ctx)
		if err != nil {
			return err
		}
		return printJSON(items)
	case "show":
		if len(args) != 2 {
			return errors.New("proposal show requires ID")
		}
		item, err := a.store.GetProposal(a.ctx, args[1])
		if err != nil {
			return err
		}
		return printJSON(item)
	case "create":
		fs := flag.NewFlagSet("proposal create", flag.ContinueOnError)
		runID := fs.String("run", "", "source run")
		scope := fs.String("scope", "run", "run, repository, or global")
		reason := fs.String("reason", "", "reason")
		diff := fs.String("diff", "", "JSON proposal contract")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *reason == "" || *diff == "" {
			return errors.New("proposal create requires --reason and --diff")
		}
		raw := json.RawMessage(*diff)
		var contract struct {
			ObservedCorrection  string          `json:"observed_correction"`
			CompatibilityImpact string          `json:"compatibility_impact"`
			TestsRequired       []string        `json:"tests_required"`
			Diff                json.RawMessage `json:"diff"`
		}
		raw = engine.RedactJSON(raw)
		if err := decodeJSON(raw, &contract); err != nil {
			return errors.New("proposal diff must be valid strict JSON")
		}
		if contract.ObservedCorrection == "" || contract.CompatibilityImpact == "" || len(contract.TestsRequired) == 0 || len(contract.Diff) == 0 {
			return errors.New("proposal requires observed_correction, compatibility_impact, tests_required, and diff")
		}
		safeReason := engine.RedactText(*reason)
		approvalArtifact := map[string]any{"run_id": *runID, "scope": *scope, "reason": safeReason, "contract": contract}
		hash, err := engine.CanonicalHash(approvalArtifact)
		if err != nil {
			return err
		}
		p := &domain.Proposal{RunID: *runID, Scope: *scope, Reason: safeReason, ArtifactHash: hash, Diff: raw}
		if err = a.store.CreateProposal(a.ctx, p); err != nil {
			return err
		}
		return printJSON(p)
	case "approve":
		if len(args) != 3 {
			return errors.New("proposal approve requires ID and hash")
		}
		if err := a.store.ApproveProposal(a.ctx, args[1], args[2]); err != nil {
			return err
		}
		return printJSON(map[string]string{"status": "approved", "id": args[1]})
	case "start-change":
		fs := flag.NewFlagSet("proposal start-change", flag.ContinueOnError)
		repo := fs.String("repo", ".", "workrun tool repository")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("proposal start-change requires proposal ID")
		}
		p, err := a.store.GetProposal(a.ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		if p.Status != "approved" {
			return errors.New("proposal must be approved before starting a change run")
		}
		abs, err := filepath.Abs(*repo)
		if err != nil {
			return err
		}
		manifestPath, err := config.FindRepository(abs)
		if err != nil {
			return errors.New("tool repository requires .agent-workflow.yaml")
		}
		_, raw, err := config.LoadRepository(manifestPath)
		if err != nil {
			return err
		}
		manifestHash := config.Hash(raw)
		if err = a.store.SaveManifest(a.ctx, manifestHash, raw); err != nil {
			return err
		}
		run, created, err := a.store.CreateOrResumeRun(a.ctx, store.CreateRunInput{Provider: "workrun-evolution", WorkItemID: p.ID, WorkItemKey: p.ID, Repository: abs, WorkflowName: a.workflow.Name, WorkflowHash: a.workflowHash, ManifestHash: manifestHash})
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"created": created, "proposal": p, "run": run})
	default:
		return errors.New("unknown proposal command")
	}
}

func (a *app) agent(args []string) error {
	if len(args) == 0 {
		return errors.New("agent requires acquire, renew, next, complete, fail, or release")
	}
	switch args[0] {
	case "acquire":
		return a.agentAcquire(args[1:])
	case "renew":
		return a.agentRenew(args[1:])
	case "next":
		return a.agentNext(args[1:])
	case "complete":
		return a.agentComplete()
	case "fail":
		return a.agentFail(args[1:])
	case "release":
		return a.agentRelease(args[1:])
	default:
		return errors.New("unknown agent command")
	}
}
func (a *app) agentAcquire(args []string) error {
	fs := flag.NewFlagSet("agent acquire", flag.ContinueOnError)
	owner := fs.String("owner", "", "session owner")
	ttl := fs.Duration("ttl", 15*time.Minute, "lease TTL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *owner == "" {
		return errors.New("acquire requires run ID and --owner")
	}
	lease, err := a.store.AcquireLease(a.ctx, fs.Arg(0), *owner, *ttl)
	if err != nil {
		return err
	}
	return printJSON(lease)
}
func (a *app) agentRenew(args []string) error {
	fs := flag.NewFlagSet("agent renew", flag.ContinueOnError)
	token := fs.String("lease", "", "lease token")
	ttl := fs.Duration("ttl", 15*time.Minute, "lease TTL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *token == "" {
		return errors.New("renew requires run ID and --lease")
	}
	lease, err := a.store.RenewLease(a.ctx, fs.Arg(0), *token, *ttl)
	if err != nil {
		return err
	}
	return printJSON(lease)
}

func (a *app) agentNext(args []string) error {
	fs := flag.NewFlagSet("agent next", flag.ContinueOnError)
	token := fs.String("lease", "", "lease token")
	host := fs.String("host", "unknown", "host name")
	caps := stringSlice{}
	fs.Var(&caps, "cap", "repeatable host capability")
	toolSchemas := stringSlice{}
	fs.Var(&toolSchemas, "tool-schema", "repeatable capability=schema-hash")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *token == "" {
		return errors.New("next requires run ID and --lease")
	}
	lease, err := a.store.ValidateLease(a.ctx, fs.Arg(0), *token)
	if err != nil {
		return err
	}
	inventory := domain.CapabilityInventory{ProtocolVersion: domain.ProtocolVersion, Host: *host, Capabilities: map[string]bool{}, ToolSchemas: map[string]string{}}
	for _, cap := range caps {
		inventory.Capabilities[cap] = true
	}
	for _, item := range toolSchemas {
		cap, hash, ok := strings.Cut(item, "=")
		if !ok || cap == "" || hash == "" {
			return fmt.Errorf("invalid --tool-schema %q; expected capability=hash", item)
		}
		inventory.ToolSchemas[cap] = hash
	}
	if path, pathErr := config.DefaultUserPath(); pathErr == nil {
		if user, _, loadErr := config.LoadUser(path); loadErr == nil {
			for cap := range inventory.Capabilities {
				if binding, ok := user.Bindings[cap]; ok {
					live := inventory.ToolSchemas[cap]
					if live == "" {
						return fmt.Errorf("capability %s requires --tool-schema for configured MCP binding", cap)
					}
					if live != binding.SchemaHash {
						return fmt.Errorf("MCP schema drift for %s: configured %s, live %s", cap, binding.SchemaHash, live)
					}
				}
			}
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}
	}
	eng, err := a.engineFor(fs.Arg(0))
	if err != nil {
		return err
	}
	if eng.User == nil {
		return errors.New("user trust configuration is required; run workrun init-user")
	}
	action, err := eng.Next(a.ctx, fs.Arg(0), *lease, inventory)
	if err != nil {
		return err
	}
	return printJSON(action)
}
func (a *app) agentComplete() error {
	var result domain.ActionResult
	if err := decodeStdin(&result); err != nil {
		return err
	}
	eng, err := a.engineFor(result.RunID)
	if err != nil {
		return err
	}
	run, applied, err := eng.Complete(a.ctx, result)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"applied": applied, "run": run})
}
func (a *app) agentFail(args []string) error {
	fs := flag.NewFlagSet("agent fail", flag.ContinueOnError)
	reason := fs.String("reason", "", "blocking reason")
	class := fs.String("class", "permanent", "failure class: transient, rate_limited, auth, validation, conflict, permanent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var result domain.ActionResult
	if err := decodeStdin(&result); err != nil {
		return err
	}
	eng, err := a.engineFor(result.RunID)
	if err != nil {
		return err
	}
	run, err := eng.Fail(a.ctx, result, *reason, *class)
	if err != nil {
		return err
	}
	return printJSON(run)
}
func (a *app) agentRelease(args []string) error {
	fs := flag.NewFlagSet("agent release", flag.ContinueOnError)
	token := fs.String("lease", "", "lease token")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 1 || *token == "" {
		return errors.New("release requires run ID and --lease")
	}
	return a.store.ReleaseLease(a.ctx, fs.Arg(0), *token)
}
func initUser(args []string) error {
	fs := flag.NewFlagSet("init-user", flag.ContinueOnError)
	timezone := fs.String("timezone", "Local", "IANA timezone or Local")
	workStart := fs.String("work-start", "09:00", "workday start")
	workEnd := fs.String("work-end", "18:00", "workday end")
	approval := fs.String("approve", "", "approved config hash prefix")
	force := fs.Bool("force", false, "replace existing config after approval")
	providers := stringSlice{}
	hosts := stringSlice{}
	fs.Var(&providers, "allow-provider", "repeatable trusted provider ID")
	fs.Var(&hosts, "allow-host", "repeatable trusted HTTPS hostname")
	maxContent := fs.Int("max-content-bytes", 1<<20, "maximum evidence payload bytes")
	retention := fs.Int("retention-days", 30, "sensitive payload retention")
	bindings := stringSlice{}
	fs.Var(&bindings, "binding", "repeatable capability=server/tool@schema-hash")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("init-user takes flags only")
	}
	cfg := config.User{SchemaVersion: 1, Protocol: ">=1.0 <2.0", Timezone: *timezone, WorkHours: config.WorkHours{Start: *workStart, End: *workEnd}, Bindings: map[string]config.Binding{}, Trust: config.TrustPolicy{AllowedProviders: providers, AllowedURLHosts: hosts, MaxContentBytes: *maxContent}, RetentionDays: *retention}
	for _, item := range bindings {
		cap, target, ok := strings.Cut(item, "=")
		if !ok {
			return fmt.Errorf("invalid binding %q", item)
		}
		toolPart, schemaHash, ok := strings.Cut(target, "@")
		if !ok {
			return fmt.Errorf("binding %q lacks @schema-hash", item)
		}
		server, tool, ok := strings.Cut(toolPart, "/")
		if !ok || cap == "" || server == "" || tool == "" || schemaHash == "" {
			return fmt.Errorf("invalid binding %q", item)
		}
		cfg.Bindings[cap] = config.Binding{Server: server, Tool: tool, SchemaHash: schemaHash}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	hash := config.Hash(raw)
	path, err := config.DefaultUserPath()
	if err != nil {
		return err
	}
	if _, statErr := os.Lstat(path); statErr == nil {
		file, dir := config.UserSecurityInfo(path)
		if !file.Safe || !dir.Safe {
			return errors.New("existing user configuration path is unsafe")
		}
		if !*force {
			return fmt.Errorf("%s already exists", path)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if *approval == "" {
		fmt.Print(string(raw))
		return printJSON(map[string]any{"path": path, "hash": hash, "approval_required": true, "next": "rerun with the same options and --approve " + hash[:12]})
	}
	if len(strings.TrimSpace(*approval)) < 8 || !strings.HasPrefix(hash, strings.TrimSpace(*approval)) {
		return errors.New("approval hash does not match generated user config")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err = os.Lstat(path); err == nil {
		file, dir := config.UserSecurityInfo(path)
		if !file.Safe || !dir.Safe {
			return errors.New("existing user configuration path is unsafe")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err = os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	return printJSON(map[string]any{"path": path, "hash": hash, "written": true})
}
func (a *app) redactedState(runID string) (map[string]any, error) {
	run, err := a.store.GetRun(a.ctx, runID)
	if err != nil {
		return nil, err
	}
	runRaw, err := json.Marshal(run)
	if err != nil {
		return nil, err
	}
	var safeRun map[string]any
	if err = json.Unmarshal(engine.RedactJSON(runRaw), &safeRun); err != nil {
		return nil, err
	}
	events, err := a.store.Events(a.ctx, runID)
	if err != nil {
		return nil, err
	}
	for i := range events {
		events[i].Data = nil
	}
	schema, err := a.store.SchemaVersion(a.ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"schema_version": schema, "protocol_version": domain.ProtocolVersion, "run": safeRun, "events": events}, nil
}

func (a *app) exportState(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("out", "", "write redacted JSON to file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("export requires run ID")
	}
	payload, err := a.redactedState(fs.Arg(0))
	if err != nil {
		return err
	}
	if *out == "" {
		return printJSON(payload)
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err = os.WriteFile(*out, raw, 0o600); err != nil {
		return err
	}
	return printJSON(map[string]any{"path": *out, "redacted": true})
}

func (a *app) diagnostics(args []string) error {
	fs := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
	out := fs.String("out", "", "diagnostic bundle JSON path")
	approval := fs.String("approve", "", "approved manifest hash prefix")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *out == "" {
		return errors.New("diagnostics requires run ID and --out")
	}
	payload, err := a.redactedState(fs.Arg(0))
	if err != nil {
		return err
	}
	manifest := map[string]any{"run_id": fs.Arg(0), "output": *out, "contents": []string{"run snapshot without source content", "event metadata without payloads", "database/protocol versions"}, "redacted": true}
	hash, err := engine.CanonicalHash(manifest)
	if err != nil {
		return err
	}
	if *approval == "" {
		return printJSON(map[string]any{"manifest": manifest, "artifact_hash": hash, "approval_required": true})
	}
	trimmed := strings.TrimSpace(*approval)
	if len(trimmed) < 8 || !strings.HasPrefix(hash, trimmed) {
		return errors.New("approval hash does not match diagnostic manifest")
	}
	raw, err := json.MarshalIndent(map[string]any{"manifest": manifest, "state": payload}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err = os.WriteFile(*out, raw, 0o600); err != nil {
		return err
	}
	return printJSON(map[string]any{"path": *out, "artifact_hash": hash, "redacted": true})
}

func (a *app) doctor(args []string) error {
	if len(args) > 0 {
		return errors.New("doctor takes no arguments")
	}
	schema, schemaErr := a.store.SchemaVersion(a.ctx)
	integrity, integrityErr := a.store.IntegrityCheck(a.ctx)
	checks := map[string]any{"version": version, "protocol": domain.ProtocolVersion, "workflow": map[string]any{"name": a.workflow.Name, "hash": a.workflowHash, "valid": true}, "database": map[string]any{"schema_version": schema, "schema_error": errorText(schemaErr), "integrity": integrity, "integrity_error": errorText(integrityErr)}}
	dbFile, dbDir := a.store.SecurityInfo()
	checks["database_security"] = map[string]any{"file": dbFile, "directory": dbDir, "safe": dbFile.Safe && dbDir.Safe}
	if path, err := config.FindRepository("."); err == nil {
		cfg, raw, loadErr := config.LoadRepository(path)
		if loadErr != nil {
			checks["repository_error"] = loadErr.Error()
		} else {
			checks["repository_manifest"] = map[string]any{"path": path, "hash": config.Hash(raw), "target_branch": cfg.TargetBranch, "required_roles": cfg.Requirements.RequiredRoles, "automatic_merge": cfg.Safety.AllowAutomaticMerge, "force_push": cfg.Safety.AllowForcePush}
		}
	} else {
		checks["repository_error"] = err.Error()
	}
	if path, err := config.DefaultUserPath(); err == nil {
		configFile, configDir := config.UserSecurityInfo(path)
		checks["user_config_security"] = map[string]any{"file": configFile, "directory": configDir, "safe": configFile.Safe && configDir.Safe}
		user, raw, loadErr := config.LoadUser(path)
		if loadErr != nil {
			checks["user_error"] = loadErr.Error()
		} else {
			probes := make([]map[string]any, 0, len(user.Bindings))
			for capability, binding := range user.Bindings {
				probes = append(probes, map[string]any{"capability": capability, "server": binding.Server, "tool": binding.Tool, "expected_schema_hash": binding.SchemaHash, "status": "live_probe_required_by_host"})
			}
			checks["user_config"] = map[string]any{"path": path, "hash": config.Hash(raw), "retention_days": user.RetentionDays, "allowed_providers": user.Trust.AllowedProviders, "allowed_url_hosts": user.Trust.AllowedURLHosts, "binding_probes": probes}
		}
	}
	return printJSON(checks)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func installAgent(args []string) error {
	fs := flag.NewFlagSet("install-agent", flag.ContinueOnError)
	dest := fs.String("dest", "", "override skill root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("install-agent requires qwen or omp")
	}
	host := fs.Arg(0)
	if host != "qwen" && host != "omp" {
		return errors.New("supported hosts: qwen, omp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	metadata := ""
	adapter := assets.OMPAdapter
	if host == "qwen" {
		adapter = assets.QwenAdapter
		metadata = "priority: 10\n"
	}
	if *dest == "" {
		if host == "qwen" {
			*dest = filepath.Join(home, ".qwen", "skills")
		} else {
			*dest = filepath.Join(home, ".omp", "agent", "skills")
		}
	}
	body := strings.ReplaceAll(assets.SkillTemplate, "{{HOST}}", host)
	body = strings.ReplaceAll(body, "{{HOST_METADATA}}", metadata)
	body = strings.ReplaceAll(body, "{{HOST_ADAPTER}}", adapter)
	dir := filepath.Join(*dest, "workrun")
	if err = os.MkdirAll(filepath.Join(dir, "references"), 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "SKILL.md")
	if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	for action, playbook := range assets.Playbooks {
		if err = os.WriteFile(filepath.Join(dir, "references", action+".md"), []byte(playbook+"\n"), 0o600); err != nil {
			return err
		}
	}
	return printJSON(map[string]string{"host": host, "skill": path})
}

type bootstrapSource struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Finding  string `json:"finding"`
}
type bootstrapEvidence struct {
	TargetBranch       string            `json:"target_branch"`
	Language           string            `json:"language,omitempty"`
	InReviewIntent     string            `json:"in_review_intent"`
	ResolvedIntent     string            `json:"resolved_intent"`
	VerificationPolicy string            `json:"verification_policy"`
	BaseUpdateStrategy string            `json:"base_update_strategy"`
	Sources            []bootstrapSource `json:"sources"`
}

func repositoryConfigFromEvidence(evidence bootstrapEvidence) config.Repository {
	return config.Repository{SchemaVersion: 1, Protocol: ">=1.0 <2.0", Language: evidence.Language, TargetBranch: evidence.TargetBranch, BranchTemplate: "task/{key}-{slug}", Requirements: config.RequirementPolicy{RequiredRoles: []string{"business", "functional"}, Traversal: []config.TraversalStep{{Kind: "description"}, {Kind: "parent"}, {Kind: "relationships", Relationships: []string{"mentioned_in", "relates", "clones"}}}, MaxDepth: 1, CandidateLimit: 20}, TrackerIntents: map[string]string{"mark_in_review": evidence.InReviewIntent, "mark_resolved": evidence.ResolvedIntent}, Verification: config.VerificationPolicy{Policy: evidence.VerificationPolicy}, BaseUpdate: config.BaseUpdatePolicy{Strategy: evidence.BaseUpdateStrategy}, Safety: config.SafetyPolicy{RequireBriefApproval: true, RequireReviewApproval: true}}
}

func initRepository(args []string) error {
	fs := flag.NewFlagSet("init-repo", flag.ContinueOnError)
	evidenceText := fs.String("evidence", "", "JSON evidence from authoritative repository files")
	force := fs.Bool("force", false, "replace existing manifest after approval")
	approval := fs.String("approve", "", "approved proposal hash prefix")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("init-repo accepts at most one repository path")
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	path := filepath.Join(abs, ".agent-workflow.yaml")
	if !*force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists", path)
		}
	}
	var evidence bootstrapEvidence
	if err = decodeJSON([]byte(*evidenceText), &evidence); err != nil {
		return errors.New("init-repo requires valid JSON --evidence")
	}
	if evidence.TargetBranch == "" || evidence.InReviewIntent == "" || evidence.ResolvedIntent == "" || evidence.VerificationPolicy == "" || evidence.BaseUpdateStrategy == "" || len(evidence.Sources) == 0 {
		return errors.New("bootstrap evidence requires target branch, tracker intents, policies, and at least one source")
	}
	for _, source := range evidence.Sources {
		if source.Path == "" || source.Revision == "" || source.Finding == "" {
			return errors.New("each bootstrap source requires path, revision, and finding")
		}
	}
	cfg := repositoryConfigFromEvidence(evidence)
	if err = cfg.Validate(); err != nil {
		return fmt.Errorf("inferred manifest is incomplete: %w", err)
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	manifestHash := config.Hash(raw)
	artifact := map[string]any{"path": path, "manifest_hash": manifestHash, "evidence": evidence}
	proposalHash, err := engine.CanonicalHash(artifact)
	if err != nil {
		return err
	}
	if *approval == "" {
		fmt.Print(string(raw))
		return printJSON(map[string]any{"proposal": artifact, "artifact_hash": proposalHash, "approval_required": true, "next": "rerun with identical --evidence and --approve " + proposalHash[:12]})
	}
	trimmed := strings.TrimSpace(*approval)
	if len(trimmed) < 8 || !strings.HasPrefix(proposalHash, trimmed) {
		return errors.New("approval hash does not match evidence-bound manifest proposal")
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	return printJSON(map[string]any{"path": path, "manifest_hash": manifestHash, "proposal_hash": proposalHash, "written": true})
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func decodeStdin(target any) error {
	dec := json.NewDecoder(os.Stdin)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("expected exactly one JSON result")
		}
		return err
	}
	return nil
}

func decodeJSON(raw []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("expected exactly one JSON object")
		}
		return err
	}
	return nil
}

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }
