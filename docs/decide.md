# decide

One decision of a known shape, asked of any `model.Provider` and checked
before it is handed back. Use it where code needs a model's judgement as
a value, not prose: a filter, a router, a grader, a form filler.

```go
d := decide.Decider{Provider: openrouter.New(key), Model: "meta/muse-spark-1.3-contributor"}
got, err := d.Decide(ctx, decide.Request{
	Kind:     decide.Choice,
	Question: "Which team should answer this ticket?",
	Context:  ticket.Body,
	Options:  decide.Values("billing", "support", "sales"),
	Explain:  true,
})
team := got.Choice() // "billing"; got.Confidence, got.Reason, got.Usage
```

| Kind | Value | Takes |
| --- | --- | --- |
| `bool` | `true` or `false` | |
| `choice` | one option's value | `options` (two or more) |
| `choices` | option values, in the order of `options` | `options`, `atLeast`, `atMost` |
| `score` | a number | `min` and `max` (0 and 1 when neither is set), `integer` |
| `rank` | every option's value, best first | `options` (two or more) |
| `extract` | an object | `schema` (a JSON Schema of type object) |

Every kind also takes `question` (required), `context` (the material,
fenced off from the question as untrusted), `criteria` (a rubric or
policy), `explain` (ask for a reason), `model` and `maxTokens`.

- **How it asks.** The model is offered one tool, `decide`, whose
  arguments are `value`, `confidence` (0 to 1) and, with `explain`,
  `reason`, which comes first so the model reasons before it answers. Tool
  choice is left to the model: some OpenRouter providers accept only
  `auto`. Arguments written as text rather than as a call are read too.
- **How it checks.** An option is matched without regard to case and
  handed back as the request spelled it; a list may name each option
  once; a rank names every option; a score is in range (and whole when
  `integer`); an extract matches its schema. `"yes"`, `"4"` and a
  double-encoded list are accepted for a bool, a number and a list.
- **Retries.** An answer that doesn't fit is asked for again with what was
  wrong with it, up to `Decider.Attempts` (default 2). Then the error is
  `ErrUndecided`. A request that can't be asked is `ErrInvalid`. A
  provider's error is returned as it is. `Decision.Usage` sums every
  attempt and `Decision.Attempts` counts them.
- **Model.** `Request.Model`, else `Decider.Model`; there is no default
  here, so a host resolves its own (the harness: `core.models.decision-model`,
  else the price sheet's low tier). `Decider.Prefs` are the provider
  routing preferences every request is sent with.

`agentcap` serves it as `llm.decide` (the request, plus `profile` for the
model and provider preferences). The harness serves it to subsystems and
pipelines as `core.models.decide@1`, and a pipeline's `flow.decide` step
calls that.

`decide/live_test.go` asks a real model for every kind:
`OPENROUTER_API_KEY=... AGENT_SDK_LIVE=1 go test ./decide/`.
