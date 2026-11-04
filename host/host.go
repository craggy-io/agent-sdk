// Package session is the runtime root (§2): config snapshot, tool view,
// trace sink, budgets, threads, handles, mailbox and event hub. One agent
// loop (§5) runs under it; loop built-ins, Starlark and (later) registry
// capabilities are adapters to the control service in control.go.
//
// Handle/mailbox code lives in this package, not session/control and
// session/mailbox subpackages: the control service needs Session internals
// (scheduler, threads, budgets) and Session needs the control service, so
// subpackages would form an import cycle (§10 keeps them as files here).
//
// ToolView is the agents-local mirror of the future registry *View (§9.3,
// deferred): Narrow/Tools/Run map 1:1 onto registry View.Narrow and
// tools.Defs/Call once those exist, and model.CallerInfo mirrors
// registry.Caller. A thin adapter will swap the implementation.
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/loop"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/trace"
	"github.com/craggy-io/agent-sdk/transcript"
)

// ToolView is a (possibly narrowed) set of tools an agent may call (§4.3).
// It satisfies loop.ToolRunner. Groups describes the groups its tools
// name; narrowing keeps them (the catalogue counts only listed tools). Narrow returns a child view admitting only
// tools the filter allows; narrowing can only shrink the set. WaitJob waits
// for a job one of its tools handed back (see loop.ToolRunner).
type ToolView interface {
	Tools() []model.Tool
	Groups() []model.ToolGroup
	Run(ctx context.Context, name string, args json.RawMessage) (model.ToolResult, error)
	WaitJob(ctx context.Context, jobID string) (model.ToolResult, bool)
	Narrow(spec model.FilterSpec) ToolView
}

// ToolFunc is one Go-function tool registered in a MapView.
type ToolFunc func(ctx context.Context, args json.RawMessage) (model.ToolResult, error)

// MapView is a ToolView over named Go functions (tests, examples, hosts).
type MapView struct {
	defs   []model.Tool
	groups []model.ToolGroup
	funcs  map[string]ToolFunc
	// Caller, when set, is invoked for tools with no registered func.
}

// NewMapView builds a MapView from tool definitions and their functions.
// Every definition name should have a func; missing funcs run as errors.
func NewMapView(defs []model.Tool, funcs map[string]ToolFunc) *MapView {
	cp := append([]model.Tool(nil), defs...)
	f := make(map[string]ToolFunc, len(funcs))
	for k, v := range funcs {
		f[k] = v
	}
	return &MapView{defs: cp, funcs: f}
}

// FuncView builds a one-tool MapView.
func FuncView(tool model.Tool, fn ToolFunc) *MapView {
	return NewMapView([]model.Tool{tool}, map[string]ToolFunc{tool.Name: fn})
}

func (v *MapView) Tools() []model.Tool { return append([]model.Tool(nil), v.defs...) }

// WithGroups sets the groups this view's tools name and returns v.
func (v *MapView) WithGroups(gs ...model.ToolGroup) *MapView {
	v.groups = append([]model.ToolGroup(nil), gs...)
	return v
}

func (v *MapView) Groups() []model.ToolGroup { return append([]model.ToolGroup(nil), v.groups...) }

func (v *MapView) Run(ctx context.Context, name string, args json.RawMessage) (model.ToolResult, error) {
	// Run enforces this view's def set, not just the func registry:
	// a narrowed view must be unable to dispatch filtered-out tools
	// even to a caller that never consults Tools() (rule 4).
	listed := false
	for _, t := range v.defs {
		if t.Name == name {
			listed = true
			break
		}
	}
	fn, ok := v.funcs[name]
	if !listed || !ok {
		return unknownTool(name), nil
	}
	return fn(ctx, args)
}

// WaitJob reports done=false: Go-function tools never hand back jobs.
func (v *MapView) WaitJob(context.Context, string) (model.ToolResult, bool) {
	return model.ToolResult{}, false
}

func (v *MapView) Narrow(spec model.FilterSpec) ToolView {
	out := &MapView{funcs: map[string]ToolFunc{}, groups: v.groups}
	for _, t := range v.defs {
		if spec.Allows(t) {
			out.defs = append(out.defs, t)
			if fn, ok := v.funcs[t.Name]; ok {
				out.funcs[t.Name] = fn
			}
		}
	}
	return out
}

// narrowedView wraps any ToolView with an additional filter, so session
// narrowing works even for views whose own Narrow is shallow.
type narrowedView struct {
	base  ToolView
	specs []model.FilterSpec
}

func narrow(v ToolView, spec model.FilterSpec) ToolView {
	if v == nil {
		return NewMapView(nil, nil)
	}
	if nv, ok := v.(*narrowedView); ok {
		return &narrowedView{base: nv.base, specs: append(append([]model.FilterSpec(nil), nv.specs...), spec)}
	}
	return &narrowedView{base: v, specs: []model.FilterSpec{spec}}
}

func (v *narrowedView) allows(t model.Tool) bool {
	for _, s := range v.specs {
		if !s.Allows(t) {
			return false
		}
	}
	return true
}

func (v *narrowedView) Groups() []model.ToolGroup { return v.base.Groups() }

func (v *narrowedView) Tools() []model.Tool {
	var out []model.Tool
	for _, t := range v.base.Tools() {
		if v.allows(t) {
			out = append(out, t)
		}
	}
	return out
}

func (v *narrowedView) Run(ctx context.Context, name string, args json.RawMessage) (model.ToolResult, error) {
	for _, t := range v.base.Tools() {
		if t.Name == name {
			if !v.allows(t) {
				break
			}
			return v.base.Run(ctx, name, args)
		}
	}
	return unknownTool(name), nil
}

// unknownTool is a view's answer to a tool it does not offer; Code stays
// unknown_tool for the trace.
func unknownTool(name string) model.ToolResult {
	res := model.Fail(model.CodeNotFound, "unknown_tool", "unknown tool: %s; call only the tools you were given", name)
	res.Code = "unknown_tool"
	return res
}

// WaitJob waits through the base view: a job handle came from a tool
// this view admitted, so narrowing has nothing to add.
func (v *narrowedView) WaitJob(ctx context.Context, jobID string) (model.ToolResult, bool) {
	return v.base.WaitJob(ctx, jobID)
}

func (v *narrowedView) Narrow(spec model.FilterSpec) ToolView { return narrow(v, spec) }

// callerCtxKey carries the model.CallerInfo for the running node so tool
// implementations can audit who called them (§2).
type callerCtxKey struct{}

// CallerFromContext returns the caller chain for the calling agent node.
func CallerFromContext(ctx context.Context) (model.CallerInfo, bool) {
	c, ok := ctx.Value(callerCtxKey{}).(model.CallerInfo)
	return c, ok
}

// ViewFromContext returns the tool-view ceiling of the calling node, so
// adapters (workflow tools.*, agentcap) can only narrow it (rule 4).
func ViewFromContext(ctx context.Context) (ToolView, bool) {
	nc, ok := nodeCtxFrom(ctx)
	if !ok || nc.view == nil {
		return nil, false
	}
	return nc.view, true
}

// RunIDFromContext returns the session run ID the calling node belongs to.
func RunIDFromContext(ctx context.Context) (string, bool) {
	nc, ok := nodeCtxFrom(ctx)
	if !ok || nc.run == nil {
		return "", false
	}
	return nc.run.id, true
}

// NodeIDFromContext returns the calling node's ID, when inside a node.
func NodeIDFromContext(ctx context.Context) (string, bool) {
	nc, ok := nodeCtxFrom(ctx)
	if !ok || nc.nodeID == "" {
		return "", false
	}
	return nc.nodeID, true
}

// Options configures a Session. Trace may be nil (in-memory). Caller is
// the root caller; its Session field is overwritten with the session ID.
type Options struct {
	ID       string // session ID; generated when empty
	Config   config.SessionConfig
	View     ToolView
	Provider model.Provider
	Defs     *agentdef.Loader
	Trace    trace.Sink
	Caller   model.CallerInfo
}

// Session is the runtime root. Open one; Close it when done.
type Host struct {
	id       string
	provider model.Provider
	defs     *agentdef.Loader
	view     ToolView
	sink     trace.Sink
	lease    trace.WriterLease
	recorder *trace.Recorder
	root     model.CallerInfo

	cfgMu sync.Mutex
	cfg   config.SessionConfig

	baseCtx context.Context
	cancel  context.CancelFunc
	epoch   string

	threads  *threadStore
	mailbox  *mailbox
	handles  *handleStore
	peers    *threadPeers
	changed  *signal // mailbox or agent state changed (§6.7 waits)
	hub      *hub
	closedMu sync.Mutex
	closed   bool
}

// WorkflowRunner is set by package workflow (blank import in cmd/tests) to
// avoid an import cycle: workflow imports session, not vice versa.
var WorkflowRunner func(ctx context.Context, s *Host, nameOrSource string, input map[string]any) (any, error)

// Open creates a Session, acquiring the single-writer lease (§7.2).
func Open(ctx context.Context, opts Options) (*Host, error) {
	if opts.Provider == nil {
		return nil, fmt.Errorf("host: nil Provider")
	}
	s := &Host{
		id: opts.ID, provider: opts.Provider, defs: opts.Defs,
		view: opts.View, sink: opts.Trace, cfg: opts.Config, epoch: model.NewID(),
	}
	if s.id == "" {
		s.id = model.NewID()
	}
	if s.view == nil {
		s.view = NewMapView(nil, nil)
	}
	if s.cfg.Profiles == nil {
		s.cfg = config.Defaults()
	}
	s.baseCtx, s.cancel = context.WithCancel(context.Background())
	s.threads = newThreadStore()
	s.mailbox = newMailbox(s)
	s.handles = newHandleStore()
	s.peers = newThreadPeers()
	s.changed = newSignal()
	s.hub = newHub(s)
	if s.sink != nil {
		lease, err := s.sink.AcquireWriter(ctx, s.id)
		if err != nil {
			s.cancel()
			return nil, fmt.Errorf("host: acquire writer lease: %w", err)
		}
		s.lease = lease
		s.recorder = trace.NewRecorder(s.sink, lease)
		// Keep the writer lease fenced-in for long sessions (§7.2).
		go func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-s.baseCtx.Done():
					return
				case <-t.C:
					_ = lease.Renew(s.baseCtx)
				}
			}
		}()
	}
	s.root = opts.Caller
	if s.root.ID == "" {
		s.root.ID = "user:root"
	}
	if s.root.Kind == "" {
		s.root.Kind = "user"
	}
	s.root.Session = s.id
	_ = s.emit(ctx, trace.Event{Type: "host.start", SessionID: s.id,
		Data: mustJSON(map[string]any{"caller": s.root.ID, "epoch": s.epoch})})
	if s.defs != nil {
		_ = s.defs.Load()
		for _, sh := range s.defs.Shadowed {
			_ = s.emit(ctx, trace.Event{Type: "agentdef.shadowed", SessionID: s.id,
				Data: mustJSON(map[string]any{"shadowed": sh})})
		}
	}
	return s, nil
}

// Close cancels live activations, releases the writer lease and stops the
// event hub. Reusable handles keep their (durable) history in the trace.
func (s *Host) Close() error {
	s.closedMu.Lock()
	if s.closed {
		s.closedMu.Unlock()
		return nil
	}
	s.closed = true
	s.closedMu.Unlock()
	s.cancel()
	s.hub.close()
	if s.lease != nil {
		return s.lease.Release(context.Background())
	}
	return nil
}

func (s *Host) isClosed() bool {
	s.closedMu.Lock()
	defer s.closedMu.Unlock()
	return s.closed
}

// ID returns the session ID. Caller returns the root caller chain.
func (s *Host) ID() string                { return s.id }
func (s *Host) Caller() model.CallerInfo  { return s.root }
func (s *Host) Defs() *agentdef.Loader    { return s.defs }
func (s *Host) View() ToolView            { return s.view }
func (s *Host) Sink() trace.Sink          { return s.sink }
func (s *Host) Recorder() *trace.Recorder { return s.recorder }
func (s *Host) Provider() model.Provider  { return s.provider }

// Config returns a copy of the current session config. Runs snapshot it
// at start (§3: a run reads config once); SetConfig affects later runs.
func (s *Host) Config() config.SessionConfig {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.cfg
}

func (s *Host) SetConfig(cfg config.SessionConfig) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.cfg = cfg
}

// ThreadItems returns a copy of a named thread ("" = primary).
func (s *Host) ThreadItems(name string) []model.Item {
	return s.threads.items(threadKey(name))
}

func threadKey(name string) string {
	if name == "" {
		return "primary"
	}
	return "thread:" + name
}

// emit appends durable events via the recorder; nil-sink safe.
func (s *Host) emit(ctx context.Context, events ...trace.Event) error {
	if s.recorder == nil {
		return nil
	}
	for i := range events {
		if events[i].Time.IsZero() {
			events[i].Time = time.Now().UTC()
		}
		if events[i].SessionID == "" {
			events[i].SessionID = s.id
		}
	}
	return s.recorder.Emit(ctx, events...)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"error":"marshal"}`)
	}
	return b
}

// threadStore owns named transcripts; one activation writes a thread at a
// time (§6.5), enforced by a per-key lock handed out by acquire.
type threadStore struct {
	mu      sync.Mutex
	threads map[string]*transcript.Transcript
	locks   map[string]*sync.Mutex
}

func newThreadStore() *threadStore {
	return &threadStore{threads: map[string]*transcript.Transcript{}, locks: map[string]*sync.Mutex{}}
}

func (ts *threadStore) get(key string) (*transcript.Transcript, *sync.Mutex) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	tr, ok := ts.threads[key]
	if !ok {
		tr = transcript.New()
		ts.threads[key] = tr
	}
	lk, ok := ts.locks[key]
	if !ok {
		lk = &sync.Mutex{}
		ts.locks[key] = lk
	}
	return tr, lk
}

func (ts *threadStore) acquire(key string) (*transcript.Transcript, func()) {
	tr, lk := ts.get(key)
	lk.Lock()
	return tr, lk.Unlock
}

func (ts *threadStore) items(key string) []model.Item {
	tr, _ := ts.get(key)
	return tr.Items()
}

func (ts *threadStore) restore(key string, items []model.Item) {
	tr, lk := ts.get(key)
	lk.Lock()
	defer lk.Unlock()
	if tr.Len() == 0 {
		tr.Append(items...)
	}
}

// Result is one agent node's outcome: the loop result plus identity.
type Result struct {
	loop.Result
	RunID    string `json:"run_id,omitempty"`
	NodeID   string `json:"node_id,omitempty"`
	HandleID string `json:"handle_id,omitempty"`
	DefName  string `json:"def,omitempty"`
}

// WfResult is a workflow run outcome (starlark-free).
type WfResult struct {
	Value  any    `json:"value,omitempty"`
	Status string `json:"status"`
	RunID  string `json:"run_id,omitempty"`
}

// RunWorkflow runs a workflow through the registered WorkflowRunner hook.
func (s *Host) RunWorkflow(ctx context.Context, nameOrSource string, input map[string]any) (WfResult, error) {
	if WorkflowRunner == nil {
		return WfResult{Status: "failed: no_workflow_engine"},
			fmt.Errorf("host: workflow engine not registered (import the workflow package)")
	}
	runID := ""
	if nc, ok := nodeCtxFrom(ctx); ok {
		runID = nc.run.id
	} else {
		run := s.newRun(ctx)
		runID = run.id
		ctx = withNodeCtx(ctx, s.rootNodeCtx(ctx, run))
		_ = s.emit(ctx, trace.Event{Type: "run.start", RunID: run.id,
			Data: mustJSON(map[string]any{"workflow": nameOrSource})})
		defer func() {
			run.end()
			_ = s.emit(context.WithoutCancel(ctx), trace.Event{Type: "run.end", RunID: run.id})
		}()
	}
	v, err := WorkflowRunner(ctx, s, nameOrSource, input)
	if err != nil {
		return WfResult{Status: "failed: workflow", Value: v, RunID: runID}, err
	}
	return WfResult{Status: loop.StatusDone, Value: v, RunID: runID}, nil
}
