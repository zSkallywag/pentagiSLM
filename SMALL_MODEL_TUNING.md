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

## Loop recovery

A small model stuck on a failing approach tends to vary its parameters (different timeouts, wrappers, the same script rewritten) rather than abandon the approach — PentAGI's exact-match repeat detector does not catch this, and invoking the adviser does not help when the adviser is the same small model. Two triggers, either of which arms the recovery directive in `smallModelStateBlock`:

1. **Grounded-failure similarity** (`smallmodel.DetectStuck`): when the last few failed attempts recorded by the success/failure classifier are mutually similar (token-set overlap), `RecoveryDirective` is prepended. This catches a loop of attempts the classifier marks as failures, but depends on the classifier and on the output reading as a clear failure.

2. **Repetition without progress** (`smallmodel.DetectStuckByRepetition`): classifier-independent and general. It catches any loop where the agent repeats one gesture while learning nothing new — a brute-force grind, re-reading a dead log, rewriting and re-running the same script. `CanonicalizeCommand` reduces each command to its shape (here-doc bodies and quoted strings become placeholders, a path keeps only its directory and extension so `/work/a_final.py` and `/work/b_final.py` collapse, every number becomes `N`), so superficial variation does not hide the repetition. The no-progress gate is the discriminator that keeps a legitimate repeated action — a brute-force or a sweep still covering new ground — from being flagged: a new deterministic fact resets the counter, so the directive fires only when the same canonical gesture repeats *and* no new fact has been learned for several commands. The directive then names the specific gesture to abandon (`RepetitionRecoveryDirective`).

Neither fixes the model's capability, only its tendency to loop; the robust fix for a mechanism gap (e.g. non-interactive ssh auth) is a first-class session tool that handles the mechanism in code. The two triggers map onto a taxonomy of behaviour — repetitive vs varied, progressing vs stuck — and together cover the "repetitive + stuck" quadrant with high precision; a "varied + stuck" loop (many different actions, none working, none repeated) is the one case left to a higher-cost pure-no-progress trigger, deliberately omitted here to avoid flagging slow but genuine exploration.

## Missing-tool grounding

A distinct loop is subtler than varying a broken approach: the model picks a *correct* tool that simply is not installed, runs it, reads `sshpass: not found`, and keeps re-running the absent tool instead of installing it. Measured on a 13B model against this exact scenario, the gap is one of salience, not capability: given the clean `not found` line in a focused prompt the model provisions the tool itself 66% of the time, and 100% with a one-line nudge to install missing tools first — but in a real flow of hundreds of turns the signal is buried in noise and lost to chain compaction, so the model looped indefinitely and never installed it.

`smallmodel.DetectMissingTools` (`missingtool.go`) lifts the signal out of the terminal output deterministically — it parses the shell's "command not found" forms (bash, sh/dash, zsh), strips ANSI colour, rejects a mistyped command reported as not-found and the always-present shells themselves — and `compactToolResponse` records a `MissingToolFact` in the structured state. The fact rides at the top of the prompt through compaction and is phrased to stay true after the tool is installed. It installs nothing: it makes the agent's own observation durable and salient, which is what moves the model from the looping 0% back to the 66–100% it reaches when it actually sees the error. Detection runs on the raw output before compaction, because a `not found` line is short and never truncated on its own but could be dropped from a large output.

## Strategy gate

The loop recovery above is a *cure* — it escapes a dead end once the agent is in it. The strategy gate is *prevention*: it keeps the agent out of a whole class of dead end a small model falls into — reaching for an expensive wordlist brute-force as a first move, even when the task already hands it a credential or before any service has been enumerated. Observed in practice: a flow spent a day grinding `hydra` against an invented wordlist on a target whose foothold credential was meant to be used directly, never getting a shell.

`smallmodel.StrategyGate` (`strategygate.go`) intercepts a wordlist credential attack before it runs. Detection is deterministic and narrow: a brute-force tool (`hydra`, `medusa`, `ncrack`, `patator`) *and* a list argument, with the short list flags matched case-sensitively so a single targeted `-p one -l user` attempt is never gated — only the expensive `-P`/`-L` form is. It gates when the structured state already holds a credential (authenticate with it first) or shows no recon yet (enumerate first), and it allows a wordlist attack once recon is done and no credential is in hand — a legitimate last resort. The intercept message replays the facts the agent already has and lists the untried alternatives (provided/default credentials, credentials from recon, key auth, anonymous access, a known CVE), so the nudge points at concrete data rather than a vague "think again". It fires at most once per task (`Store.StrategyGateFired`): the agent is asked to reconsider a single time, then trusted.

The credential precondition is fed by a deterministic, keyword-anchored extractor in `Facts` (`extract.go`): a labelled `user:pass` pair found in tool output becomes a `credential seen:` fact. A credential *given in the task description* only gates once it is likewise present in the state, so feeding task-context credentials into the state is the natural follow-up that makes the gate bite on the exact flow that motivated it.

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
