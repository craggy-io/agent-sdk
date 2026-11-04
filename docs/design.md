# Design

The numbered sections below are the ones code comments cite as `§N`.
They state the invariants, not the implementation; per-package guides
say how each package keeps them.

## 1. Shape

A Go library, not a server. One host owns config, provider, tool view,
definitions and trace. Agents and workflows are Starlark definitions;
the ReAct loop, budgets, journaling and tool dispatch are Go. Every
effect a script has goes through a host built-in or a capability.

## 2. Runtime (`host`)

One host per process (or per tenant): a config snapshot, a tool view,
a trace sink under a single-writer lease, thread transcripts, agent
handles with a durable-input mailbox, and an event hub. Runs form a
tree under one shared budget charged before work. Loop built-ins,
workflow built-ins and capabilities are adapters over the same
control service, so authority checks live in one place.

## 3. Configuration (`config`)

Profiles are model bindings; definitions name profiles, never models.
Effective config is layered (session, then workflow, definition, call
site): limits take the minimum, loop fields take the last set value,
profile names remap per layer and fall back to `default` when unknown
instead of failing. Config is Go values; file formats live in
adapters.

## 4. Definitions (`agentdef`)

Agents are Starlark `agent(...)` declarations with single-parent
`extends`. Prompt fragments append, replace or remove by name; tool
filters union and `max_effect` only tightens; skills and delegates
union; loop fields override one by one and the output schema replaces.
Resolution renders one system prompt (templates with
`missingkey=error`, skills catalogue last) and records provenance for
every field. Dynamic definitions must extend a loaded one.

Tool filters: include/exclude globs, a max-effect cap, and an allow
list exempt from the cap only. Effects rank read-only, then
idempotent, then mutating (long ranks as mutating); unknown effects
fail closed.

## 5. The loop (`loop`)

One loop for every agent: pre-checks (budget, cancellation,
max-steps), then steps of project (normalise the transcript), sample
(streaming, buffered until the stream ends), classify (a truncated,
errored or unfinished sample never dispatches; an empty completion is
never success), and dispatch. Results settle exactly once and append
contiguously in call order; read-only calls may run concurrently,
the rest run serially in order. Transient failures retry within a
run-level budget; context overflow compacts and retries once.

Compaction summarises covered ranges into summary items; oversized
tool results spill behind `result_read`; old results clear to markers
before compaction is needed. Budget runway notices tell the agent to
wrap up at configured fractions of each limit. Statuses are stable
strings (`done`, `cancelled`, `stopped: ...`, `failed: <code>`,
`blocked: recovery_required`).

Progressive tool disclosure advertises only always-loaded and
ungrouped tools plus a catalogue; `tools_load` adds a group's tools.

## 6. Workflows and control (`workflow`, `host`)

A workflow is a `WORKFLOW = workflow(...)` manifest (input schema,
agent allowlist, profile remap, tightened limits) plus `run(input)`.
Built-ins run agents (`agent`, `spec`, `parallel`, `start`, `wait`),
call tools (`call`, `tools.*`), nest workflows (`workflow_run`), gate
on humans (`ask`), and drive handles (`spawn`, `send`, `interrupt`,
`resume`, `close`, `status`, `list_agents`).

Durability is replay-from-top: every observable built-in call gets a
call key; intent is journaled before dispatch and the result before
the value returns to the script. Resume re-executes the pinned source
with the journal loaded — recorded results, including failures,
return as-is and only missing calls run. The start pin records source
and input blobs, the config hash and the runtime version; any
mismatch refuses the resume. A tool operation with an intent and no
outcome blocks with `recovery_required` until a host resolves it
(read-only operations may simply retry). `ask` suspends the run; an
answer plus resume continues it.

Handles are addressable agents with owners. Input delivery is a
durable receipt (accepted is not seen); modes are `message`, `steer`
and `follow_up`. A parent joins (waits for) agents it started before
stopping. Threads carry conversations across runs; delegated runs get
fresh ephemeral threads.

## 7. Trace (`trace`)

Everything observable is an append-only event with a cursor, plus
content-addressed blobs. A step request is recorded as ordered item
hashes plus a params blob, so the exact provider request can be
rebuilt byte for byte. One writer holds the lease. Sinks: in-memory,
and SQLite in `adapters/tracesqlite`. The trace is the workflow
journal, the crash-recovery source, and the usage/performance input —
one record, three readers.

## 8. Provider (`model`, `openrouter`)

The loop's only model seam is `Sample` (streaming, with deltas) plus
`Model` (context window and pricing). Requests carry profiles'
reasoning, fallbacks, cache and provider preferences; responses carry
usage with cost. One provider ships (OpenRouter); tests use scripted
and chaos providers from `agenttest`.

## 9. Capabilities (`agentcap`, `host.RegistryView`)

Tools are capability-services capabilities. The host's tool view is a
registry view; definition filters intersect with it, and nested calls
never widen past the caller's view. The SDK registers its own
surface (`agents.*`, `workflows.*`, `sessions.*`, `recovery.*`,
`trace.*`, `llm.*`, `memory.*`) the same way, so scripts, agent tool
loops, MCP and HTTP share one registry. Adding a capability to the
registry is the whole integration: new Starlark scripts see it as a
built-in, agents see it as a tool, with no host changes.

Runtime registration is session-scoped (`define`) until a host
approves the exact bytes (`promote`), which writes a `.star` file
into a definition root.

## 10. Layout

Flat public packages at the module root; heavy dependencies (SQLite,
YAML) quarantined in `adapters/` nested modules; a debug CLI in
`cmd/craggy-agent` and a runnable host in `examples/host`, each a
nested module. Dependencies point one way: `model`, `transcript`,
`config`, `trace` at the bottom; `loop`; then `host`; `workflow` and
`agentcap` on top. Nothing here imports a consumer.

## 11. Concurrency

A semaphore bounds runnable activations per run tree. A node that
blocks awaiting delegates releases its slot and reacquires it after,
so concurrency 1 cannot deadlock on delegation.

## 13. Testing invariants

Transcript invariants (append-only, contiguous tool results, summary
coverage) are checked by `transcript.Check` (§13.1), exercised with
scripted providers (§13.2), chaos fault injection and fuzzing
(§13.3), and crash-window journal tests (§13.5). Loop, workflow and
journal invariants are stated in §5–§6 and tested where they live.
