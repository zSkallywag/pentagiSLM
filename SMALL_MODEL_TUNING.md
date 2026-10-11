# Small-model tuning

This document maps the nine-point small-model guide to the code that implements it. Everything is gated behind `SMALL_MODEL_MODE` (off by default): with the flag off, PentAGI behaves exactly as before.

## Turning it on

Set in `.env` (see `.env.example` for the full list):

```
SMALL_MODEL_MODE=true
SMALL_MODEL_CTX_WINDOW=32768
SMALL_MODEL_CTX_BUDGET_PERCENT=40
SMALL_MODEL_TOOL_OUTPUT_MAX_BYTES=6144
SMALL_MODEL_VERIFIER_ENABLED=true
SMALL_MODEL_SCOPE=10.0.0.5/32          # authorized targets; empty disables scope checks
SMALL_MODEL_DENIED_COMMANDS=           # extra destructive substrings, comma-separated
SMALL_MODEL_FEWSHOT_FILE=              # path to a JSON array of {input, output}; empty disables few-shot
SMALL_MODEL_FEWSHOT_K=3
```

With the profile on, the execution monitor is enabled automatically so hard cases escalate to the `adviser` agent — point that role at a larger model. Set `EXECUTION_MONITOR_ENABLED=false` to opt out.

With `SMALL_MODEL_MODE=true`, the `SUMMARIZER_*` thresholds the operator has not set explicitly are lowered to small-model starting points (`applySmallModelProfile` in `backend/pkg/config/config.go`). An explicit `SUMMARIZER_*` value always wins.

## Point-by-point

1. **Short context by choice.** `applySmallModelProfile` lowers the summarizer thresholds; `smallmodel.Budget` (`backend/pkg/smallmodel/budget.go`) enforces a token budget as a fraction of the declared window, triggered before each model call in `performAgentChain` (`backend/pkg/providers/performer.go`) via `smallModelCompactIfOverBudget`. Token counting uses a prudent bytes-per-token estimate and accepts a real tokenizer through `Budget.CountTokens`.
2. **Structured state instead of history.** `smallmodel.Store` / `State` (`backend/pkg/smallmodel/state.go`) hold objective, facts, failed attempts and next step, built by code, not the model. The block is prepended to the execution context on every turn (`performAgentChain`), so it survives chain compaction. Facts are fed in deterministically from tool output (point 4).
3. **Summaries without the small model.** Deterministic extraction (`smallmodel.Facts` in `extract.go`) pulls IPs, ports, service banners and CVEs with regexes — zero cost, zero drift — and feeds the state. For the remaining free-text chain summary, assign a more capable model to the `summarizer` agent role in PentAGI's per-agent model configuration; the summarize handler already routes through that role, so no code change is needed.
4. **Pre-process tool output.** `smallmodel.CompactOutput` (`toolsummary.go`) parses nmap `-oX` XML into a few lines, keeps head and tail of other long output (the useful part of a log is often at the end), and returns the extracted facts. Wired into `execToolCall` right after a successful tool execution, before the output enters the chain.
5. **Reliable tool calls.** `sanitizeToolArgs` (`backend/pkg/providers/small_model.go`) runs `cast.SanitizeJSONControlChars` on every tool call's arguments before execution in small-model mode, keeping the sanitized form only when it is valid JSON. The existing execution-error repair path (`fixToolCallArgs`) remains the second line. Ollama's structured-output fallback is already enabled in the provider.
6. **Small tasks, fresh contexts.** PentAGI already runs specialized agents (pentester, coder, searcher, …). The `smallmodel.Store` is keyed by `{flow, task}`, so facts found in one subtask carry into the next through state rather than through a long shared history — state is the channel between agents.
7. **Explicit repair of orphaned tool calls.** `FallbackResponseContent` (`backend/pkg/cast/chain_ast.go`) now tells the model the call may have partially executed and to verify state before retrying, instead of "please try again".
8. **A verifier at the critical points.** `smallmodel.Verifier` (`verifier.go`) classifies commands before execution: destructive patterns and out-of-scope targets become `Confirm` and are blocked fail-closed in `execToolCall` with an explanatory tool response. Scope comes from `SMALL_MODEL_SCOPE`.
9. **Measure.** See `small-model-eval/` for the methodology, metrics, and scenario format.

## Harness additions

On top of the nine points, three items from the general small-model harness playbook are wired here:

- **Constrained decoding.** Already present in PentAGI: the JSON-producing agents run on `OptionsTypeSimpleJSON`, which applies `llms.WithJSONMode()` (json_object), and `tool_call_fixer` uses it. The finer json_schema form is deliberately avoided for provider compatibility (see the deepseek/glm notes in `pkg/providers/openaicompat`), so the small-model profile relies on json_object plus the sanitize→validate→repair path rather than schema-forcing every call, which would break the tool-call protocol.
- **Escalation to a bigger model.** `applySmallModelProfile` enables and tightens the execution monitor (`EXECUTION_MONITOR_*`) when the operator has not set it. When the agent repeats a tool or runs long, PentAGI invokes the adviser (mentor) agent — assign a larger model to the `adviser` role and hard cases are reviewed by it, the easy majority staying on the small model. Explicit `EXECUTION_MONITOR_*` values still win.
- **Dynamic few-shot.** `smallmodel.ExampleSet` (`fewshot.go`) selects the examples most similar to the current situation (token-overlap ranking) from a JSON file named by `SMALL_MODEL_FEWSHOT_FILE`, and `performAgentChain` prepends up to `SMALL_MODEL_FEWSHOT_K` of them to the prompt. Empty file disables it.

## Decision classifier (offload discrete choices)

Some decisions in the pipeline are closed-set classifications, not generation — picking a container image, and later allow/deny or success/failure. A small generative model is slow and can ramble or hallucinate on these. `smallmodel.Classifier` (`classifier.go`) offloads them to a zero-shot classifier served over HTTP, pointed at by `SMALL_MODEL_CLASSIFIER_URL`.

Wired today for container-image selection: when the profile is on and the URL is set, `NewFlowProvider` asks the classifier to choose the image from a labelled candidate set instead of calling the generative model, and falls back to the model if the classifier is unreachable. A reference sidecar (a DeBERTa-v3 zero-shot model behind FastAPI) and the image-selection benchmark live in `small-model-eval/classifier-sidecar/`. On that benchmark the classifier matched the generative model's accuracy while running on CPU, ~2× faster and deterministically.

It also judges tool-output success/failure: `compactToolResponse` classifies each tool result and, on a high-confidence failure, records a grounded entry in the state's failed attempts, so the state reflects what actually failed rather than the model's narrative. This is an advisory signal, not a hard gate — on its benchmark the classifier and the generative model were equally accurate (and made different mistakes), so the two disagreeing is itself a useful escalation trigger.

## Execution-context stamping

A recurring small-model failure is losing track of where a command ran: it runs a bare command on the local sandbox (root by default), reads the result, and mistakes it for a command run on a remote target. This is general — it applies to any multi-context situation (ssh, docker exec into another container, a database shell, a chroot, multiple hosts), and it is what made an agent fabricate a captured flag from a local file.

The fix makes the execution context an explicit, code-managed fact instead of something the model infers: `compactToolResponse` stamps every terminal result with `smallmodel.ExecContextNote` (`execcontext.go`), telling the model the command ran in the PentAGI sandbox as root and that only ssh/remote wrappers reach a target. A bare `id` returning root is then unmistakably the sandbox, not the target. The deeper form of this — first-class session/target handles and context-aware verification of claimed actions — is the next step.

## Where the code lives

- `backend/pkg/smallmodel/` — the self-contained logic (budget, state, extraction, tool-output compaction, verifier) with unit tests.
- `backend/pkg/providers/small_model.go` — the thin wiring between `flowProvider` and the package.
- `backend/pkg/providers/performer.go` — the three call sites (state block, budget compaction, per-call sanitize + verify + compact).
- `backend/pkg/config/config.go` — the `SMALL_MODEL_*` knobs and the profile.
- `backend/pkg/cast/chain_ast.go` — the improved orphan-call repair message.

## Build note

The changes compile against this tree but were not built here (no Go toolchain in the authoring environment). Before relying on them, run from `backend/`:

```
go build ./...
go test ./pkg/smallmodel/... ./pkg/config/... ./pkg/cast/... ./pkg/csum/... ./pkg/providers/...
```
