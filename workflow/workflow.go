// Package workflow runs Starlark workflows (design §6): a WORKFLOW
// manifest plus run(input), the §6.2 built-ins, call-key journaling in
// the trace, and Resume replay (§6.3). It registers itself as
// host.WorkflowRunner on import, so host.RunWorkflow and the
// loop's opt-in `workflow` tool (§6.4) reach it without an import cycle.
//
// Replay model: every observable built-in gets a call key
// (node path | built-in | per-node ordinal | argument hash). A wf.call
// intent is committed before dispatch and its result before the value
// returns to Starlark. Resume re-executes the script with the journal
// loaded: recorded results (including failures) are returned as-is and
// only missing calls run. Pure helpers (render, json, schema) are not
// journaled. Tool calls made through call/tools.* additionally record
// operation.intent/outcome; an intent with no outcome is reconciled per
// §6.3 (read-only may retry, anything else blocks with
// recovery_required until recovery.resolve).
package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"go.starlark.net/starlark"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/internal/jschema"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/script-services/dialect"
	"github.com/craggy-io/script-services/script"
	"github.com/craggy-io/script-services/starcompile"
)

// RuntimeVersion is pinned into every wf.start (§6.3); a mismatch on
// Resume stops with resume_incompatible.
const RuntimeVersion = "agent-sdk/workflow-1"

// Status strings for workflow outcomes (mirrors §5.7 spellings).
const (
	StatusDone               = "done"
	StatusRecoveryRequired   = "blocked: recovery_required"
	StatusResumeIncompatible = "failed: resume_incompatible"
	StatusCancelled          = "cancelled"
)

// Options tunes a workflow execution.
type Options struct {
	// RunID pins the workflow run ID (generated when empty). Resume
	// sets it to the original run.
	RunID string
	// Resume replays the journal of RunID instead of starting fresh.
	Resume bool
	// NodePath is the journal node path prefix for nested runs.
	NodePath string
	// View overrides the tool view the workflow's own tool calls
	// (tools.*, call, agent check= capabilities) run through. It
	// does not widen what agents the workflow runs can see: those
	// still resolve against the session/caller view, narrowed by
	// definition and tools= filters.
	View host.ToolView
	// Limits bounds Starlark execution (steps and wall clock). The
	// zero value takes script.DefaultLimits — never unbounded.
	Limits script.Limits
}

// Result is one workflow execution outcome.
type Result struct {
	Value  any    `json:"value,omitempty"` // JSON-shaped run() return value
	Status string `json:"status"`
	RunID  string `json:"run_id"`
	// Pending is the unanswered ask() gate when Status is
	// StatusWaiting; nil otherwise.
	Pending *PendingInput `json:"pending,omitempty"`
}

func init() { host.WorkflowRunner = Run }

// Register (re)installs the workflow engine as host.WorkflowRunner.
// The package does this in init; Register is for hosts that blank-import
// conditionally or replaced the hook.
func Register() { host.WorkflowRunner = Run }

// Run executes a workflow by catalogued name (defs loader), by source
// text (anything containing a newline or "WORKFLOW"), and returns its
// run() value as JSON-shaped Go data.
func Run(ctx context.Context, s *host.Host, nameOrSource string, input map[string]any) (any, error) {
	res, err := RunEx(ctx, s, nameOrSource, input, Options{})
	if err != nil {
		return res.Value, err
	}
	return res.Value, nil
}

// RunEx is Run with options; it also returns the workflow run ID so the
// caller can Resume later.
func RunEx(ctx context.Context, s *host.Host, nameOrSource string, input map[string]any, opts Options) (Result, error) {
	wf, source, err := resolveSource(ctx, s, nameOrSource)
	if err != nil {
		return Result{Status: "failed: workflow_not_found"}, err
	}
	return execute(ctx, s, wf, source, input, opts)
}

// Resume replays a previous workflow run (§6.3): it loads the pinned
// source/input/config from the trace, refuses on any pin mismatch
// (resume_incompatible), restores session projections, then re-executes
// the script returning journaled results for completed calls.
func Resume(ctx context.Context, s *host.Host, runID string) (Result, error) {
	return ResumeEx(ctx, s, runID, Options{})
}

// ResumeEx is Resume with options (notably a workflow tool View, so
// a resumed run's own tool calls resolve exactly as the first
// pass's did).
func ResumeEx(ctx context.Context, s *host.Host, runID string, opts Options) (Result, error) {
	if s.Sink() == nil {
		return Result{Status: StatusResumeIncompatible, RunID: runID},
			fmt.Errorf("workflow: resume %s: session has no trace sink", runID)
	}
	pin, err := loadPin(ctx, s.Sink(), runID)
	if err != nil {
		return Result{Status: StatusResumeIncompatible, RunID: runID}, err
	}
	if pin.RuntimeVersion != RuntimeVersion {
		return Result{Status: StatusResumeIncompatible, RunID: runID},
			fmt.Errorf("workflow: resume %s: runtime version %q, want %q", runID, pin.RuntimeVersion, RuntimeVersion)
	}
	srcBlob, err := s.Sink().GetBlob(ctx, pin.SourceBlob)
	if err != nil {
		return Result{Status: StatusResumeIncompatible, RunID: runID}, fmt.Errorf("workflow: resume %s: source blob: %w", runID, err)
	}
	if string(trace.HashOf(srcBlob)) != string(pin.SourceBlob) || hashBytes(srcBlob) != pin.SourceHash {
		return Result{Status: StatusResumeIncompatible, RunID: runID},
			fmt.Errorf("workflow: resume %s: source hash mismatch", runID)
	}
	// A catalogued workflow whose file changed since the pin stops the
	// resume instead of silently running new code (§6.3). One that left
	// the catalogue replays its pinned source, with nothing to load().
	wf := &agentdef.Workflow{Name: pin.Name}
	if pin.Name != "" && s.Defs() != nil {
		if cur, ok := s.Defs().Workflow(pin.Name); ok {
			if hashBytes([]byte(cur.Source())) != pin.SourceHash {
				return Result{Status: StatusResumeIncompatible, RunID: runID},
					fmt.Errorf("workflow: resume %s: workflow %q changed since the run started", runID, pin.Name)
			}
			wf = cur
		}
	}
	wf.Description = pin.Description
	if got := configHash(s); got != pin.ConfigHash {
		return Result{Status: StatusResumeIncompatible, RunID: runID},
			fmt.Errorf("workflow: resume %s: session config changed since the run started", runID)
	}
	inBlob, err := s.Sink().GetBlob(ctx, pin.InputBlob)
	if err != nil {
		return Result{Status: StatusResumeIncompatible, RunID: runID}, fmt.Errorf("workflow: resume %s: input blob: %w", runID, err)
	}
	var input map[string]any
	if len(inBlob) > 0 && string(inBlob) != "null" {
		if err := json.Unmarshal(inBlob, &input); err != nil {
			return Result{Status: StatusResumeIncompatible, RunID: runID}, fmt.Errorf("workflow: resume %s: input: %w", runID, err)
		}
	}
	// Restore threads/mailbox/handles so recorded handles exist again.
	_ = s.Rebuild(ctx, "")
	return execute(ctx, s, wf, string(srcBlob), input, Options{RunID: runID, Resume: true, View: opts.View})
}

// resolveSource returns the catalogued workflow nameOrSource names (after
// a reload), or an empty Workflow and the source text itself for ad-hoc
// source.
func resolveSource(ctx context.Context, s *host.Host, nameOrSource string) (*agentdef.Workflow, string, error) {
	trimmed := strings.TrimSpace(nameOrSource)
	if strings.Contains(trimmed, "\n") || strings.Contains(trimmed, "WORKFLOW") || strings.HasPrefix(trimmed, "#") {
		return &agentdef.Workflow{}, nameOrSource, nil
	}
	if s.Defs() != nil {
		_, _ = s.Defs().Reload(ctx) // a failed reload keeps the previous catalogue
		if wf, ok := s.Defs().Workflow(trimmed); ok {
			return wf, wf.Source(), nil
		}
	}
	return nil, "", fmt.Errorf("workflow: unknown workflow %q", trimmed)
}

func configHash(s *host.Host) string {
	b, _ := json.Marshal(s.Config())
	return hashBytes(b)
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hash16(v any) string {
	b, _ := json.Marshal(v)
	return hashBytes(b)[:16]
}

// exec is one workflow execution (live or replaying).
type exec struct {
	ctx     context.Context
	s       *host.Host
	wf      *agentdef.Workflow // catalogued name ("" for ad-hoc source) and script (nil Unit when not catalogued)
	source  string
	runID   string // workflow run ID (journal scope)
	nodeID  string // workflow node ID in the trace
	path    string // journal node path
	resume  bool
	lim     script.Limits
	journal *journal
	view    host.ToolView
	pending *PendingInput // set by ask() when it suspends this run

	man     *manifest
	phase   string
	spentMu sync.Mutex
	spent   float64
	steps   int

	mu       sync.Mutex
	ordinals map[string]int // builtin -> count, on the Starlark thread
}

func (e *exec) nextKey(builtin string, args any) string {
	e.mu.Lock()
	if e.ordinals == nil {
		e.ordinals = map[string]int{}
	}
	e.ordinals[builtin]++
	ord := e.ordinals[builtin]
	e.mu.Unlock()
	return fmt.Sprintf("%s|%s|%d|%s", e.path, builtin, ord, hash16(args))
}

func (e *exec) emit(typ string, data map[string]any) error {
	if e.journal == nil || e.journal.rec == nil {
		return nil
	}
	b, _ := json.Marshal(data)
	return e.journal.rec.Emit(e.ctx, trace.Event{
		Type: typ, SessionID: e.s.ID(), RunID: e.runID, NodeID: e.nodeID,
		Data: b,
	})
}

func (e *exec) noteUsage(u model.Usage) {
	e.spentMu.Lock()
	e.spent += u.CostUSD
	e.spentMu.Unlock()
}

func execute(ctx context.Context, s *host.Host, wf *agentdef.Workflow, source string, input map[string]any, opts Options) (Result, error) {
	e := &exec{ctx: ctx, s: s, wf: wf, source: source, resume: opts.Resume,
		lim: opts.Limits, runID: opts.RunID, nodeID: model.NewID()}
	if e.runID == "" {
		// Prefer the enclosing session run so trace tree queries join.
		if rid, ok := host.RunIDFromContext(ctx); ok && opts.NodePath == "" {
			e.runID = rid
		} else {
			e.runID = model.NewID()
		}
	}
	// On resume the journal scope stays the original run even when the
	// enclosing session run differs.
	if opts.Resume {
		e.runID = opts.RunID
	}
	e.path = opts.NodePath
	if e.path == "" {
		e.path = wf.Name
		if e.path == "" {
			e.path = "wf:" + hashBytes([]byte(source))[:8]
		}
	}
	e.view = s.View()
	if opts.View != nil {
		e.view = opts.View
	}
	if v, ok := host.ViewFromContext(ctx); ok {
		e.view = v // rule 4: an agent-written workflow sees the agent's view
	}

	var rec *trace.Recorder
	if s.Recorder() != nil {
		rec = s.Recorder()
	}
	e.journal = newJournal(rec, s.Sink(), e.runID)

	if opts.Resume {
		if err := e.journal.load(ctx); err != nil {
			return Result{Status: "failed: journal", RunID: e.runID}, err
		}
	} else if rec != nil && opts.NodePath == "" {
		// Only the top-level workflow pins wf.start; nested runs share
		// the journal run and must not replace the resume pin.
		if err := e.pinStart(ctx, input); err != nil {
			return Result{Status: "failed: trace", RunID: e.runID}, err
		}
	}

	thread := script.NewThread("workflow:"+e.path, e.limits(), nil)
	thread.SetLocal("wf", e)
	thread.SetLocal("ctx", ctx)
	globals, err := e.loadScript(ctx, thread)
	if err != nil {
		_ = e.emit("wf.end", map[string]any{"status": "failed: parse", "error": err.Error()})
		return Result{Status: "failed: parse", RunID: e.runID}, err
	}
	if err := e.validateInput(input); err != nil {
		_ = e.emit("wf.end", map[string]any{"status": "failed: input", "error": err.Error()})
		return Result{Status: "failed: input", RunID: e.runID}, err
	}
	// Honour the accepted input even when the caller passed nil.
	if input == nil {
		input = map[string]any{}
	}
	// Record the resolved manifest pins that only exist after load.
	if !opts.Resume {
		_ = e.emit("wf.manifest", map[string]any{"manifest": e.man.json()})
	}

	out, err := script.Run(ctx, e.limits(), thread, func() (starlark.Value, error) {
		return starlark.Call(thread, globals["run"], starlark.Tuple{toStar(input)}, nil)
	})
	if err != nil {
		if e.pending != nil {
			// Suspended at an ask() gate: the journal holds every
			// completed call, Resume replays to this point.
			_ = e.emit("wf.end", map[string]any{"status": StatusWaiting, "key": e.pending.Key, "kind": e.pending.Kind})
			return Result{Status: StatusWaiting, RunID: e.runID, Pending: e.pending}, nil
		}
		status := "failed: workflow"
		if ctx.Err() != nil {
			status = StatusCancelled
		}
		if isRecovery(err) {
			status = StatusRecoveryRequired
		}
		_ = e.emit("wf.end", map[string]any{"status": status, "error": err.Error()})
		return Result{Status: status, RunID: e.runID}, err
	}
	val, err := fromStar(out)
	if err != nil {
		_ = e.emit("wf.end", map[string]any{"status": "failed: result", "error": err.Error()})
		return Result{Status: "failed: result", RunID: e.runID}, err
	}
	_ = e.emit("wf.end", map[string]any{"status": StatusDone})
	return Result{Value: val, Status: StatusDone, RunID: e.runID}, nil
}

// pin is the wf.start payload (§6.3 replay pins).
type pin struct {
	Name           string     `json:"name,omitempty"`
	Description    string     `json:"description,omitempty"`
	SourceHash     string     `json:"source_hash"`
	SourceBlob     trace.Hash `json:"source_blob"`
	InputBlob      trace.Hash `json:"input_blob"`
	ConfigHash     string     `json:"config_hash"`
	RuntimeVersion string     `json:"runtime_version"`
	Path           string     `json:"path"`
}

func (e *exec) pinStart(ctx context.Context, input map[string]any) error {
	srcHash, err := e.journal.rec.PutBlob(ctx, []byte(e.source))
	if err != nil {
		return err
	}
	inJSON, _ := json.Marshal(input)
	inHash, err := e.journal.rec.PutBlob(ctx, inJSON)
	if err != nil {
		return err
	}
	p := pin{Name: e.wf.Name, Description: e.wf.Description, SourceHash: hashBytes([]byte(e.source)), SourceBlob: srcHash,
		InputBlob: inHash, ConfigHash: configHash(e.s), RuntimeVersion: RuntimeVersion, Path: e.path}
	b, _ := json.Marshal(p)
	return e.journal.rec.Emit(ctx, trace.Event{Type: "wf.start", SessionID: e.s.ID(),
		RunID: e.runID, NodeID: e.nodeID, Data: b})
}

func loadPin(ctx context.Context, sink trace.Sink, runID string) (*pin, error) {
	evs, err := scanEvents(ctx, sink, func(ev trace.Event) bool {
		return ev.Type == "wf.start" && ev.RunID == runID
	})
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, fmt.Errorf("workflow: resume %s: no wf.start pin in trace", runID)
	}
	var p pin
	if err := json.Unmarshal(evs[len(evs)-1].Data, &p); err != nil {
		return nil, fmt.Errorf("workflow: resume %s: pin: %w", runID, err)
	}
	return &p, nil
}

// limits returns the Starlark execution limits for this run: the
// manifest-agnostic defaults (5M steps, 10 minutes) unless Options
// overrode them. Time inside blocking built-ins (agent and tool calls)
// still counts against Limits.Timeout through the context deadline
// script.Run installs.
func (e *exec) limits() script.Limits {
	if e.lim == (script.Limits{}) {
		return script.DefaultLimits
	}
	return e.lim
}

func (e *exec) loadScript(ctx context.Context, thread *starlark.Thread) (starlark.StringDict, error) {
	var globals starlark.StringDict
	var filename string
	if u := e.wf.Unit; u != nil {
		// A catalogued workflow runs its checked script: load() is served
		// by its bundle and declared dependencies (starcompile). Modules
		// see the same predeclared built-ins; their journaled calls share
		// this exec.
		filename = u.Path()
		pre := predeclared(e)
		ex, err := u.Exec(ctx, starcompile.ExecOptions{
			Predeclared: pre, ModulePredeclared: pre, Thread: thread, Mutable: true,
		})
		if err != nil {
			return nil, err // includes line numbers (§6.4)
		}
		globals = ex.Globals
	} else {
		// Ad-hoc source (or a pinned workflow no longer catalogued) has
		// no bundle, so nothing to load().
		filename = "<workflow-source>"
		if e.wf.Name != "" {
			filename = "workflows/" + e.wf.Name + ".star"
		}
		_, err := script.Run(ctx, e.limits(), thread, func() (starlark.Value, error) {
			g, err := starlark.ExecFileOptions(dialect.FileOptions, thread, filename, e.source, predeclared(e))
			globals = g
			return starlark.None, err
		})
		if err != nil {
			return nil, err // includes line numbers (§6.4)
		}
	}
	wf, ok := globals["WORKFLOW"]
	if !ok {
		return nil, fmt.Errorf("workflow: %s: missing WORKFLOW = workflow(...)", filename)
	}
	mv, ok := wf.(*manifestValue)
	if !ok {
		return nil, fmt.Errorf("workflow: %s: WORKFLOW must come from workflow()", filename)
	}
	if globals["run"] == nil {
		return nil, fmt.Errorf("workflow: %s: missing def run(input)", filename)
	}
	e.man = mv.man
	if e.man == nil {
		e.man = &manifest{}
	}
	if e.wf.Description != "" {
		e.man.Description = e.wf.Description
	}
	return globals, nil
}

func (e *exec) validateInput(input map[string]any) error {
	if e.man == nil || len(e.man.Schema) == 0 {
		return nil
	}
	schema := e.man.inputSchema()
	b, _ := json.Marshal(input)
	if b == nil {
		b = []byte("{}")
	}
	if input == nil {
		b = []byte("{}")
	}
	if err := jschema.Validate(schema, b); err != nil {
		return fmt.Errorf("workflow: input: %w", err)
	}
	return nil
}

// layers returns the manifest's config layers for agents in this
// subtree (§3.2: profile remap, tightened limits).
func (e *exec) layers() []config.Layer {
	if e.man == nil {
		return nil
	}
	l := e.man.layer("workflow:" + e.path)
	if l == nil {
		return nil
	}
	return []config.Layer{*l}
}

// isRecovery reports whether err is the recovery_required blocker.
func isRecovery(err error) bool {
	return err != nil && strings.Contains(err.Error(), "recovery_required")
}

type recoveryError struct{ op string }

func (e *recoveryError) Error() string {
	return "blocked: recovery_required: operation " + e.op + " has an unknown outcome"
}
