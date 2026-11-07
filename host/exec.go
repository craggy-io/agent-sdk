package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
)

// ExecSpec is one agent execution request. Workflow and agentcap build
// these; RunAgent is the top-level convenience wrapper.
type ExecSpec struct {
	Name    string               // definition name (empty when Def is set)
	Def     *agentdef.Definition // dynamic definition (§4.4; must extend a loaded def)
	Task    string               // user task appended to the thread
	Profile string               // call-site profile override (§3.2)
	Thread  string               // named thread; "" = primary at top level, fresh when delegated
	Tools   *model.FilterSpec    // call-site narrowing (§3.2)
	Vars    map[string]any       // template vars
	Layers  []config.Layer       // workflow-chain layers (§3.2)
	// Output, when set, is a call-site structured-output contract:
	// this run completes as structured against this JSON Schema,
	// replacing any loop.output the definition resolved.
	Output json.RawMessage
	// OutputCheck, with OutputCheckRetries, is the per-run in-loop
	// validator for structured completions (loop.OutputCheck).
	OutputCheck        loop.OutputCheck
	OutputCheckRetries int

	// internal wiring (handles)
	handleID   string
	conv       string // the handle's conversation (§6.7)
	nodeID     string // pre-allocated activation node (spawn returns it immediately)
	prevNode   string // previous activation node (§6.5)
	threadKey  string // overrides Thread when set
	mailTarget string // mailbox target; defaults to the thread key
	armFollow  bool   // activation started by a follow-up: deliver it first
}

// RunOption tweaks an ExecSpec for RunAgent.
type RunOption func(*ExecSpec)

// WithThread runs on a named thread instead of the primary thread.
func WithThread(name string) RunOption { return func(e *ExecSpec) { e.Thread = name } }

// WithProfile overrides the profile at the call site.
func WithProfile(p string) RunOption { return func(e *ExecSpec) { e.Profile = p } }

// WithVars supplies template variables.
func WithVars(v map[string]any) RunOption { return func(e *ExecSpec) { e.Vars = v } }

// WithTools narrows tools at the call site.
func WithTools(f model.FilterSpec) RunOption {
	return func(e *ExecSpec) { cp := f; e.Tools = &cp }
}

// WithOutput imposes a per-run structured-output contract: this
// run completes through the loop's complete tool against schema
// (a JSON Schema document), whatever completion the definition
// itself declares. The validated value lands in Result.Output
// and the complete call's summary in Result.Text. An empty schema
// leaves the definition's own completion untouched. The schema is
// copied; invalid JSON fails the run before any sampling.
func WithOutput(schema json.RawMessage) RunOption {
	return func(e *ExecSpec) { e.Output = append(json.RawMessage(nil), schema...) }
}

// WithOutputCheck installs a per-run validator for structured
// completions: after complete's output passes schema validation,
// check may reject it with feedback that goes back to the model as
// a notice while the run continues, up to maxRetries rejections;
// the next completion after that is accepted unchecked. maxRetries
// <= 0 disables the check. A check error fails the run like a
// BeforeStop hook error. See loop.OutputCheck.
func WithOutputCheck(check loop.OutputCheck, maxRetries int) RunOption {
	return func(e *ExecSpec) { e.OutputCheck, e.OutputCheckRetries = check, maxRetries }
}

// RunAgent runs one agent to completion via the implicit agent-turn (§2).
// nameOrDef is a definition name, a agentdef.Definition, or *agentdef.Definition.
func (s *Host) RunAgent(ctx context.Context, nameOrDef any, task string, opts ...RunOption) (Result, error) {
	spec := ExecSpec{Task: task}
	switch v := nameOrDef.(type) {
	case string:
		spec.Name = v
	case agentdef.Definition:
		cp := v
		spec.Def = &cp
	case *agentdef.Definition:
		spec.Def = v
	default:
		return Result{}, fmt.Errorf("host: RunAgent wants a definition name or agentdef.Definition, got %T", nameOrDef)
	}
	for _, o := range opts {
		o(&spec)
	}
	return s.ExecAgent(ctx, spec)
}

// runState is one run tree: config snapshot, shared budget, semaphore (§11).
type runState struct {
	id     string
	cfg    config.SessionConfig
	budget *runBudget
	sem    *semaphore
	ended  atomic.Bool
}

// end marks the run over: handles it started are woken into fresh runs.
func (r *runState) end() { r.ended.Store(true) }

func (s *Host) newRun(_ context.Context) *runState {
	cfg := s.Config() // snapshot once per run (§3)
	capN := cfg.Limits.Concurrency
	return &runState{
		id: model.NewID(), cfg: cfg,
		budget: newRunBudget(cfg.Limits),
		sem:    newSemaphore(capN),
	}
}

// nodeCtx is the execution lineage carried in ctx: run, depth, caller
// chain, tool-view ceiling, delegate allowlist and slot ownership, plus
// its messaging identity (§6.7): conversation, own address and inbox,
// and the handles it started or woke.
type nodeCtx struct {
	run       *runState
	nodeID    string
	parentID  string
	depth     int
	caller    model.CallerInfo
	owner     string // caller ID that owns handles spawned from here
	view      ToolView
	delegates []string
	workflows bool
	layers    []config.Layer
	slot      *slotRef
	handleID  string
	conv      string
	address   string // agent_send address; "" for an ephemeral node
	inbox     string // mailbox target
	awaits    *awaitSet
}

// rootNodeCtx is the lineage root of a new run started by the root caller.
func (s *Host) rootNodeCtx(ctx context.Context, run *runState) *nodeCtx {
	return &nodeCtx{run: run, caller: s.root, owner: s.root.ID, view: s.view,
		conv: conversationFrom(ctx), awaits: newAwaitSet()}
}

// detachNode strips the calling node from ctx, keeping its other values
// (host scope etc.): an activation started on a sender's behalf is not
// that sender's child.
func detachNode(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, nodeCtxKey{}, (*nodeCtx)(nil))
	return context.WithValue(ctx, callerCtxKey{}, nil)
}

type nodeCtxKey struct{}

func withNodeCtx(ctx context.Context, nc *nodeCtx) context.Context {
	ctx = context.WithValue(ctx, nodeCtxKey{}, nc)
	return context.WithValue(ctx, callerCtxKey{}, nc.caller)
}

func nodeCtxFrom(ctx context.Context) (*nodeCtx, bool) {
	nc, ok := ctx.Value(nodeCtxKey{}).(*nodeCtx)
	return nc, ok && nc != nil
}

// semaphore bounds runnable activations (§11). Zero cap means unlimited
// (still tracked so release/reacquire stay symmetric).
type semaphore struct {
	ch chan struct{}
}

func newSemaphore(capN int) *semaphore {
	if capN <= 0 {
		return &semaphore{}
	}
	return &semaphore{ch: make(chan struct{}, capN)}
}

// acquire takes a slot, calling onWait once if it has to block.
func (sm *semaphore) acquire(ctx context.Context, onWait func()) error {
	if sm.ch == nil {
		return ctx.Err()
	}
	select {
	case sm.ch <- struct{}{}:
		return nil
	default:
	}
	if onWait != nil {
		onWait()
	}
	select {
	case sm.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sm *semaphore) release() {
	if sm.ch == nil {
		return
	}
	select {
	case <-sm.ch:
	default:
	}
}

// slotRef tracks one node's slot so a parent awaiting a child can release
// and reacquire it (§11: concurrency=1 must not deadlock).
type slotRef struct {
	mu   sync.Mutex
	sem  *semaphore
	held bool
}

func (r *slotRef) releaseIfHeld() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held {
		r.sem.release()
		r.held = false
		return true
	}
	return false
}

func (r *slotRef) reacquire(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.held {
		// Use a detached wait: the parent must regain its slot to settle
		// even when ctx is cancelled; the semaphore drains as children end.
		_ = r.sem.acquire(context.WithoutCancel(ctx), nil)
		r.held = true
	}
}

// runBudget is the run-tree budget (§5.7): charged before work, shared by
// every node in the tree. It implements loop.Budget.
type runBudget struct {
	mu        sync.Mutex
	limits    config.Limits
	start     time.Time
	steps     int
	toolCalls int
	retries   int
	spentUSD  float64
}

func newRunBudget(l config.Limits) *runBudget { return &runBudget{limits: l, start: time.Now()} }

func (b *runBudget) ChargeStep(estInputTokens int, price float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.deadlineLocked(); err != nil {
		return err
	}
	if b.limits.USD > 0 {
		est := b.spentUSD
		if price > 0 && estInputTokens > 0 {
			est += float64(estInputTokens) * price
		}
		if est > b.limits.USD || b.spentUSD >= b.limits.USD {
			return &loop.BudgetError{Which: "usd"}
		}
	}
	if b.limits.Steps > 0 && b.steps+1 > b.limits.Steps {
		return &loop.BudgetError{Which: "steps"}
	}
	b.steps++
	return nil
}

func (b *runBudget) ChargeToolCall() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limits.ToolCalls > 0 && b.toolCalls+1 > b.limits.ToolCalls {
		return &loop.BudgetError{Which: "tool_calls"}
	}
	b.toolCalls++
	return nil
}

func (b *runBudget) ChargeUSD(usd float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spentUSD += usd
}

func (b *runBudget) RetriesLeft() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.retries < 10
}

func (b *runBudget) NoteRetry() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.retries++
}

func (b *runBudget) DeadlineOK() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deadlineLocked()
}

func (b *runBudget) deadlineLocked() error {
	if b.limits.WallClock > 0 && time.Since(b.start) > b.limits.WallClock {
		return &loop.BudgetError{Which: "wall_clock"}
	}
	return nil
}

func (b *runBudget) Runway() loop.Runway {
	b.mu.Lock()
	defer b.mu.Unlock()
	return loop.Runway{Limits: b.limits, SpentUSD: b.spentUSD, Elapsed: time.Since(b.start),
		Steps: b.steps, ToolCalls: b.toolCalls}
}

func (b *runBudget) snapshot() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{
		"usd_limit": b.limits.USD, "usd_spent": b.spentUSD,
		"steps": b.steps, "steps_limit": b.limits.Steps,
		"tool_calls": b.toolCalls, "tool_calls_limit": b.limits.ToolCalls,
		"depth_limit": b.limits.Depth, "concurrency": b.limits.Concurrency,
	}
}

// ExecAgent executes one agent node. With no nodeCtx in ctx it starts a
// new run (run.start/run.end, fresh budget/semaphore, config snapshot).
func (s *Host) ExecAgent(ctx context.Context, spec ExecSpec) (Result, error) {
	if s.isClosed() {
		return Result{}, fmt.Errorf("host: closed")
	}
	parent, inRun := nodeCtxFrom(ctx)
	top := !inRun
	var run *runState
	if top {
		run = s.newRun(ctx)
		parent = s.rootNodeCtx(ctx, run)
		_ = s.emit(ctx, trace.Event{Type: "run.start", RunID: run.id,
			Data: mustJSON(map[string]any{"workflow": "agent-turn", "agent": specName(spec)})})
		defer func() {
			run.end()
			_ = s.emit(context.WithoutCancel(ctx), trace.Event{Type: "run.end", RunID: run.id,
				Data: mustJSON(run.budget.snapshot())})
		}()
	} else {
		run = parent.run
	}
	return s.execNode(ctx, parent, spec, top)
}

func specName(spec ExecSpec) string {
	if spec.Name != "" {
		return spec.Name
	}
	if spec.Def != nil {
		return spec.Def.Name
	}
	return ""
}

func (s *Host) execNode(ctx context.Context, parent *nodeCtx, spec ExecSpec, top bool) (Result, error) {
	run := parent.run
	depth := parent.depth
	if !top {
		depth++
	}
	if run.cfg.Limits.Depth > 0 && depth > run.cfg.Limits.Depth {
		return Result{Result: loop.Result{Status: "stopped: budget(depth)"}, RunID: run.id},
			fmt.Errorf("host: depth cap %d exceeded", run.cfg.Limits.Depth)
	}

	// Resolve the definition (§4).
	if s.defs == nil {
		return Result{}, fmt.Errorf("host: no definition loader")
	}
	// Pick up edits to the definition layers. A failed reload keeps the
	// previous catalogue; the trace says why.
	if _, err := s.defs.Reload(ctx); err != nil {
		_ = s.emit(ctx, trace.Event{Type: "agentdef.reload_failed", SessionID: s.id,
			Data: mustJSON(map[string]any{"error": err.Error()})})
	}
	var resolved *agentdef.Resolved
	var err error
	if spec.Def != nil {
		resolved, err = s.defs.ResolveDynamic(*spec.Def, spec.Vars)
	} else {
		resolved, err = s.defs.Resolve(spec.Name, spec.Vars)
	}
	if err != nil {
		return Result{}, err
	}
	if len(spec.Output) > 0 && !json.Valid(spec.Output) {
		return Result{}, fmt.Errorf("host: WithOutput schema is not valid JSON")
	}

	// Effective config (§3.2): run snapshot ⊕ layers ⊕ call site.
	layers := append(append([]config.Layer(nil), parent.layers...), spec.Layers...)
	wantProfile := spec.Profile
	if wantProfile == "" {
		wantProfile = resolved.Profile
	}
	eff := config.Resolve(run.cfg, layers, wantProfile)
	nodeID := spec.nodeID
	if nodeID == "" {
		nodeID = model.NewID()
	}
	if eff.FellBack {
		_ = s.emit(ctx, trace.Event{Type: "config.fallback", RunID: run.id, NodeID: nodeID,
			Data: mustJSON(map[string]any{"want": wantProfile, "got": "default", "def": resolved.Name})})
	}

	// Tool view: parent ceiling ∩ definition filter ∩ call-site filter (§4.3).
	view := parent.view
	if view == nil {
		view = s.view
	}
	view = narrow(view, resolved.Tools)
	if spec.Tools != nil {
		view = narrow(view, *spec.Tools)
	}

	// Thread (§2 chat continuity; §6.5 handle threads).
	tKey := spec.threadKey
	switch {
	case tKey != "":
	case spec.Thread != "":
		tKey = threadKey(spec.Thread)
	case top:
		tKey = threadKey("")
	default:
		tKey = "ephemeral:" + nodeID // delegated agents start fresh
	}
	mailTarget := spec.mailTarget
	if mailTarget == "" {
		mailTarget = tKey
	}

	// Messaging identity (§6.7): a handle is addressed by its ID, a
	// thread's agent by the thread; a fresh delegated agent has no
	// address. The conversation is inherited, else the host's, else
	// this thread (or run, for an ephemeral node).
	conv := spec.conv
	if conv == "" {
		conv = parent.conv
	}
	address := spec.handleID
	isThread := address == "" && !strings.HasPrefix(tKey, "ephemeral:")
	if isThread {
		address = threadAddress(tKey)
	}
	if conv == "" {
		conv = tKey
		if !isThread && spec.handleID == "" {
			conv = "run:" + run.id
		}
	}
	caller := model.CallerInfo{
		ID: "agent:" + resolved.Name + "/" + nodeID, Kind: "agent", Session: s.id,
		Parent: callerPtr(parent.caller),
		Attrs:  map[string]string{"run": run.id, "node": nodeID, "address": address},
	}
	nc := &nodeCtx{
		run: run, nodeID: nodeID, parentID: parent.nodeID, depth: depth,
		caller: caller, owner: caller.ID, view: view,
		delegates: resolved.Delegates, workflows: resolved.Workflows,
		layers: layers, handleID: spec.handleID,
		slot: &slotRef{sem: run.sem},
		conv: conv, address: address, inbox: mailTarget, awaits: newAwaitSet(),
	}
	nodeCtxVal := withNodeCtx(ctx, nc)

	// Acquire an execution slot (§11); the parent released its own slot
	// before calling us when it awaits this child (see delegate).
	_ = s.emit(nodeCtxVal, trace.Event{Type: "node.start", RunID: run.id, NodeID: nodeID,
		Data: mustJSON(s.nodeStartData(resolved, view, parent, nc, eff, tKey, spec))})
	if h, err := s.putResolvedBlob(nodeCtxVal, resolved); err == nil {
		_ = s.emit(nodeCtxVal, trace.Event{Type: "agentdef.resolved", RunID: run.id, NodeID: nodeID,
			Data: mustJSON(map[string]any{"def": resolved.Name, "hash": string(h),
				"provenance": resolved.Provenance, "profile": eff.ProfileName,
				"config_sources": eff.Sources})})
	}
	if err := run.sem.acquire(nodeCtxVal, func() {
		_ = s.emit(nodeCtxVal, trace.Event{Type: "node.waiting", RunID: run.id, NodeID: nodeID,
			Data: mustJSON(map[string]any{"reason": "concurrency"})})
	}); err != nil {
		_ = s.emit(context.WithoutCancel(nodeCtxVal), trace.Event{Type: "node.end", RunID: run.id, NodeID: nodeID,
			Data: mustJSON(map[string]any{"status": loop.StatusCancelled})})
		return Result{Result: loop.Result{Status: loop.StatusCancelled}, RunID: run.id, NodeID: nodeID, DefName: resolved.Name}, nil
	}
	nc.slot.held = true
	defer func() { nc.slot.releaseIfHeld() }()

	thread, releaseThread := s.threads.acquire(tKey)
	defer releaseThread()

	s.mailbox.setActivation(mailTarget, nodeID)
	defer s.mailbox.setActivation(mailTarget, "")
	var res loop.Result
	if isThread {
		s.peers.start(tKey, conv, resolved.Name, nodeID)
		defer func() {
			s.peers.end(tKey, Result{Result: res, RunID: run.id, NodeID: nodeID, DefName: resolved.Name})
			s.changed.fire()
		}()
	}

	policy := policyFrom(eff, resolved, spec)
	act := &activation{target: mailTarget, armed: spec.armFollow}
	in := loop.Input{
		NodeID: nodeID, RunID: run.id, SessionID: s.id, DefName: resolved.Name,
		System: resolved.SystemPrompt, Thread: thread, Task: spec.Task,
		Policy: policy, Profile: eff.Profile, ProfileName: eff.ProfileName,
		OnDelta: func(d model.Delta) { s.hub.publishDelta(nodeID, run.id, d) },
		DeliverPending: func() []model.Item {
			return s.mailbox.drain(nodeCtxVal, thread, act)
		},
		HasFollowUp: func() bool {
			if s.mailbox.hasFollowUp(mailTarget) {
				act.arm()
				return true
			}
			return false
		},
		Join: func(ctx context.Context) ([]model.Item, bool) {
			return s.join(ctx, nc, act)
		},
		Skills: resolved.SkillBodies,
	}
	res, runErr := loop.Run(nodeCtxVal, loop.Deps{
		Provider: s.provider, Tools: view, Trace: s.recorder,
		Budget: run.budget, Delegate: &delegateImpl{s: s, nc: nc},
	}, in)
	status := res.Status
	if runErr != nil && status == "" {
		status = "failed: trace"
	}
	_ = s.emit(context.WithoutCancel(nodeCtxVal), trace.Event{Type: "node.end", RunID: run.id, NodeID: nodeID,
		Data: mustJSON(map[string]any{"status": status, "steps": res.Steps,
			"usage": res.Usage, "thread": tKey, "handle": spec.handleID})})
	return Result{Result: res, RunID: run.id, NodeID: nodeID,
		HandleID: spec.handleID, DefName: resolved.Name}, runErr
}

func callerPtr(c model.CallerInfo) *model.CallerInfo {
	cp := c
	return &cp
}

func (s *Host) nodeStartData(resolved *agentdef.Resolved, view ToolView, parent, nc *nodeCtx, eff config.Effective, tKey string, spec ExecSpec) map[string]any {
	tools := view.Tools()
	names := make([]string, 0, len(tools))
	hashes := map[string]string{}
	for _, t := range tools {
		names = append(names, t.Name)
		hashes[t.Name] = trace.ToolHash(t)
	}
	return map[string]any{
		"kind": "agent", "parent": parent.nodeID, "def": resolved.Name,
		"thread": tKey, "handle": spec.handleID, "previous_node": specPrevNode(spec),
		"profile": eff.ProfileName, "model": eff.Profile.Model,
		"config_sources": eff.Sources, "tools": names, "tool_hashes": hashes,
		"filter": resolved.Tools, "budgets": parent.run.budget.snapshot(),
		"delegates": resolved.Delegates, "workflows": resolved.Workflows,
		"conversation": nc.conv, "address": nc.address,
	}
}

// specPrevNode is set via handle activations through execNode's spec copy
// in control.go (kept in a field-free way: encoded in threadKey specs).
func specPrevNode(spec ExecSpec) string { return spec.prevNode }

func (s *Host) putResolvedBlob(ctx context.Context, r *agentdef.Resolved) (trace.Hash, error) {
	if s.recorder == nil {
		return "", fmt.Errorf("no sink")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return s.recorder.PutBlob(ctx, b)
}

// policyFrom builds loop.Policy: session loop defaults (via Effective)
// overridden field-by-field by the resolved definition (§3.2, §4.2),
// then by a call-site output contract when one was supplied: it
// replaces the resolved loop.output and forces structured completion.
func policyFrom(eff config.Effective, r *agentdef.Resolved, spec ExecSpec) loop.Policy {
	output := spec.Output
	p := loop.Policy{
		MaxSteps: eff.Loop.MaxSteps, ToolResultBytes: eff.Loop.ToolResultBytes,
		CompactAt: eff.Loop.CompactAt, KeepRecentTokens: eff.Loop.KeepRecentTokens,
		SummaryTokens: eff.Loop.SummaryTokens, BudgetNoticeAt: eff.Loop.BudgetNoticeAt,
	}
	ls := r.Loop
	if ls.MaxSteps != nil {
		p.MaxSteps = *ls.MaxSteps
	}
	if ls.Completion != nil {
		p.Completion = *ls.Completion
	}
	if ls.ToolResultBytes != nil {
		p.ToolResultBytes = *ls.ToolResultBytes
	}
	if ls.WaitJobs != nil {
		p.WaitJobs = *ls.WaitJobs
	}
	if ls.CompactAt != nil {
		p.CompactAt = *ls.CompactAt
	}
	if ls.KeepRecentTokens != nil {
		p.KeepRecentTokens = *ls.KeepRecentTokens
	}
	if ls.SummaryTokens != nil {
		p.SummaryTokens = *ls.SummaryTokens
	}
	if ls.ProgressiveTools != nil {
		p.ProgressiveTools = *ls.ProgressiveTools
	}
	if ls.ClearResultsAt != nil {
		p.ClearResultsAt = *ls.ClearResultsAt
	}
	if ls.KeepRecentResults != nil {
		p.KeepRecentResults = *ls.KeepRecentResults
	}
	if len(r.OutputSchema) > 0 {
		p.Output = r.OutputSchema
		if p.Completion == "" {
			p.Completion = "structured"
		}
	}
	if len(output) > 0 {
		p.Output = output
		p.Completion = "structured"
	}
	p.OutputCheck = spec.OutputCheck
	p.OutputCheckRetries = spec.OutputCheckRetries
	return p
}
