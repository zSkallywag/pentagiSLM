# PentAGI-SLM

**PentAGI, hardened for small local models.**

PentAGI-SLM is a fork of [vxcontrol/pentagi](https://github.com/vxcontrol/pentagi), the autonomous AI penetration-testing agent, that makes it usable with small language models (roughly 8B–32B, served locally by Ollama or vLLM). It adds an opt-in **small-model mode** that wraps the agent loop in deterministic, code-managed guardrails: a context budget, a structured state the model cannot forget, pre-processed tool output, pre-execution guards, loop detection with escalation, and a CPU classifier for closed-set decisions.

> **This is not weight fine-tuning.** No model is trained here. The "tuning" is in the harness around the model: what it sees, what it is allowed to run, and what happens when it gets stuck. Any model PentAGI already supports can be dropped in.

Everything is gated behind `SMALL_MODEL_MODE` (off by default). With the flag off, this fork behaves exactly like upstream PentAGI.

> [!WARNING]
> PentAGI runs real offensive tooling. Use it only against systems you own or are explicitly authorized to test, and set `SMALL_MODEL_SCOPE` so the verifier keeps the agent on target.

---

## Contents

- [Why small models fail in agent loops](#why-small-models-fail-in-agent-loops)
- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [The mechanisms](#the-mechanisms)
- [Measuring it](#measuring-it)
- [Repository layout](#repository-layout)
- [Build and test](#build-and-test)
- [Known gaps and next steps](#known-gaps-and-next-steps)
- [Credits and license](#credits-and-license)

---

## Why small models fail in agent loops

A large model can carry a long pentest session in its context and correct itself. A small one cannot: the gap between a good and a bad harness is far wider than the gap between two models. Every mechanism in this fork answers a failure that was observed in real runs.

| Failure observed with a small model | What small-model mode does |
|---|---|
| Context fills up; early findings (open ports, versions) are lost to compaction | A token budget enforced before every model call, plus a **structured state** built by code and re-injected every turn, so facts survive compaction |
| Raw tool output (nmap, long logs) floods the window | nmap XML is parsed into a few lines, other long output is cut to head + tail, and IPs, ports, banners and CVEs are extracted by regex |
| Malformed JSON in tool-call arguments | Arguments are sanitized and validated before execution; the existing repair path stays as a second line |
| Writes a correct tool call as message text (a ` ```json ` block) instead of a structured call, until the reflector gives up and the task aborts | Calls written as text are recovered and run as real tool calls, through the same guards |
| Keeps varying a dead approach (new timeout, new wrapper, same script) instead of abandoning it | Commands are reduced to their canonical shape; repetition **without new facts** triggers a recovery directive, then blocks the gesture outright if the model ignores it |
| Runs a tool that isn't installed (`sshpass: not found`) over and over | The "command not found" signal is lifted into the state, where it stays visible above compaction |
| Reads `id` → `root` in its own sandbox and believes it owns the target (one run fabricated a captured flag this way) | Every terminal result is stamped with where it actually ran |
| Opens with a wordlist brute-force, even when the task hands it a credential (one flow ground `hydra` for a day) | A strategy gate holds back `-P`/`-L` wordlist attacks until recon is done and no credential is in hand |
| A "do not stop until…" instruction runs forever (one subtask: 935 commands over several days) | A hard per-subtask command budget forces the agent to report and conclude |
| Drifts out of scope or proposes destructive commands | A fail-closed verifier checks scope and destructive patterns before anything runs |
| Slow, rambling or hallucinated answers to closed-set choices (which container image?) | A zero-shot classifier on CPU makes the choice instead, with fallback to the model |

## How it works

```mermaid
flowchart TD
    ST["Structured state<br/>objective · facts · failed attempts · missing tools"] --> PR
    FS["Few-shot examples<br/>(most similar K)"] --> PR
    PR["Prompt assembly"] --> BU{"Over context<br/>budget?"}
    BU -- yes --> CO["Compact chain<br/>(state survives)"] --> LM
    BU -- no --> LM["Small model"]
    LM --> TC["Tool call → sanitize JSON args"]
    TC --> GU{"Pre-execution guards<br/>scope · destructive · strategy gate<br/>blocked gesture · subtask budget"}
    GU -- rejected --> EX["Explained back to the model<br/>as the tool response"] --> LM
    GU -- allowed --> RU["Execute in sandbox"]
    RU --> PP["Post-processing<br/>compact output · extract facts · stamp exec context<br/>detect missing tools · classify success/failure"]
    PP --> ST
    PP --> LD{"Stuck?<br/>same gesture, no new facts"}
    LD -- yes --> RE["Recovery directive → block gesture<br/>→ adviser on a bigger model"] --> ST
```

The design principle throughout: **anything that can be decided by code is decided by code.** The small model plans and chooses actions; facts, budgets, scope, and loop detection are deterministic.

## Quick start

These steps assume you already know how to run upstream PentAGI. For installer details, provider setup, and the web UI, see the [upstream documentation](https://github.com/vxcontrol/pentagi#readme).

### 1. Build the image from this fork

The published `vxcontrol/pentagi` image does not contain these changes, so build locally:

```bash
git clone https://github.com/zSkallywag/pentagiSLM.git
cd pentagiSLM
./scripts/build-image.sh local/pentagi:slm
```

### 2. Configure `.env`

```bash
cp .env.example .env
```

Then set at least:

```bash
PENTAGI_IMAGE=local/pentagi:slm

# Small-model mode
SMALL_MODEL_MODE=true
SMALL_MODEL_CTX_WINDOW=32768            # the real context window of your model
SMALL_MODEL_SCOPE=10.0.0.5/32           # authorized targets: IPs, CIDRs, hostnames

# Your local model
OLLAMA_SERVER_URL=http://<your-ollama-host>:11434
OLLAMA_SERVER_CONFIG_PATH=/opt/pentagi/conf/ollama.provider.yml
PENTAGI_OLLAMA_SERVER_CONFIG_PATH=./my-ollama.provider.yml
```

Everything else has a small-model default (see [Configuration](#configuration)).

### 3. Put a bigger model on the `adviser` and `simple` roles

Small-model mode escalates hard cases to PentAGI's `adviser` agent, and chain summaries are written by the `simple` role (the summarizer and reporter both run on it). If those roles run the same small model, escalation is just a second opinion from the same model and summaries inherit its mistakes. Start from one of the files in [`examples/configs/`](examples/configs/) and override those two roles (model names below are examples):

```yaml
# my-ollama.provider.yml (excerpt)
primary_agent:
  model: "qwen3:14b"          # the small model doing the work
  temperature: 0.2
  max_tokens: 4000

pentester:
  model: "qwen3:14b"
  temperature: 0.2
  max_tokens: 4000

adviser:
  model: "qwen3:32b"          # larger model for escalation
  temperature: 0.3
  max_tokens: 8000

simple:
  model: "qwen3:32b"          # larger model for summaries and reports
  temperature: 0.2
  max_tokens: 8000
```

### 4. (Optional) Start the decision classifier

A DeBERTa-v3 zero-shot classifier behind FastAPI, CPU-only so it never competes with the GPU serving your agent model. The model is baked into the image at build time.

```bash
cd small-model-eval/classifier-sidecar
docker build -t pentagi-classifier .
docker run -d --name pentagi-classifier --network pentagi-network -p 8077:8077 pentagi-classifier
curl http://localhost:8077/health
```

Then in `.env`:

```bash
SMALL_MODEL_CLASSIFIER_URL=http://pentagi-classifier:8077
```

(Create the network first with `docker network create pentagi-network` if PentAGI is not running yet, or start the sidecar after `docker compose up`.)

### 5. Run

```bash
docker compose up -d
docker compose logs pentagi | grep "small-model profile active"
```

At boot the backend logs one line with the resolved profile (verifier, subtask cap, classifier URL, context window, scope), so you can confirm the mode is actually on.

## Configuration

All variables are already wired through `docker-compose.yml` and listed in `.env.example`.

| Variable | Default | What it does |
|---|---|---|
| `SMALL_MODEL_MODE` | `false` | Master switch. When off, nothing below has any effect. |
| `SMALL_MODEL_CTX_WINDOW` | `32768` | Declared context window of your model, in tokens. |
| `SMALL_MODEL_CTX_BUDGET_PERCENT` | `40` | Compact the chain when the prompt exceeds this share of the window. |
| `SMALL_MODEL_BYTES_PER_TOKEN` | `3` | Conservative bytes-per-token estimate used to count tokens. |
| `SMALL_MODEL_TOOL_OUTPUT_MAX_BYTES` | `6144` | Maximum tool output that enters the chain after compaction. |
| `SMALL_MODEL_VERIFIER_ENABLED` | `true` | Scope/destructive verifier and strategy gate. Only an explicit `false` turns them off. |
| `SMALL_MODEL_SUBTASK_MAX_COMMANDS` | `150` | Terminal commands per subtask before it must conclude. `0` disables the cap. |
| `SMALL_MODEL_SCOPE` | empty | Comma-separated IPs, CIDRs or hostnames the agent may target. Empty disables scope checks. |
| `SMALL_MODEL_DENIED_COMMANDS` | empty | Extra destructive substrings, comma-separated, added to the built-in list (`rm -rf /`, `mkfs`, `dd if=`, `shutdown`, …). |
| `SMALL_MODEL_FEWSHOT_FILE` | empty | Path to a JSON array of `{"input": …, "output": …}` examples. Empty disables few-shot. |
| `SMALL_MODEL_FEWSHOT_K` | `3` | How many of the most similar examples are prepended each turn. |
| `SMALL_MODEL_CLASSIFIER_URL` | empty | Base URL of the classifier sidecar. Empty keeps those decisions on the model. |

**What the profile changes for you.** When `SMALL_MODEL_MODE=true`, settings you leave blank are set to small-model starting points. Any value you set explicitly always wins.

| Setting | Upstream default | Small-model profile |
|---|---|---|
| `SUMMARIZER_LAST_SEC_BYTES` | 50 KB | 22 KB |
| `SUMMARIZER_MAX_BP_BYTES` | 16 KB | 7 KB |
| `SUMMARIZER_MAX_QA_SECTIONS` | 10 | 4 |
| `SUMMARIZER_MAX_QA_BYTES` | 64 KB | 22 KB |
| `EXECUTION_MONITOR_ENABLED` | off | on |
| `EXECUTION_MONITOR_SAME_TOOL_LIMIT` | 5 | 3 |
| `EXECUTION_MONITOR_TOTAL_TOOL_LIMIT` | 10 | 8 |

Blank and unset are treated the same, because docker-compose passes unset variables as empty strings.

## The mechanisms

A short tour. [`SMALL_MODEL_TUNING.md`](SMALL_MODEL_TUNING.md) has the full design notes with file and function references.

**Context budget and structured state.** Before each model call the chain is measured against `CTX_BUDGET_PERCENT` of the window and compacted if needed. A code-built state block (objective, established facts, failed attempts, missing tools, next step) is prepended on every turn, so it survives any compaction. The state is keyed by flow and task, which makes it the channel between PentAGI's specialized agents instead of a long shared history.

**Deterministic facts and tool-output compaction.** Tool output is processed before it enters the chain: nmap `-oX` XML becomes a few lines, other long output keeps its head and tail (the useful part of a log is often at the end), and regexes pull IPs, ports, service banners, CVEs and labelled `user:pass` credentials into the state. No model is involved, so nothing drifts.

**Tool-call hygiene.** Every tool call's arguments are sanitized for JSON control characters before execution, keeping the sanitized form only if it is valid JSON. When a tool call is orphaned, the repair message now tells the model the call may have partially run and to verify state before retrying, instead of "please try again".

**Tool-call recovery.** A small model often knows which tool to call and writes correct JSON for it, but sends it as message text: in a ` ```json ` block, inside `<tool_call>` tags, or bare. Upstream treats that as prose and hands it to the reflector, and the same model tends to fail every reflector round the same way until the chain aborts. In small-model mode, a reply with no structured tool calls is searched for JSON shaped `{"name": …, "arguments": {…}}` (also `parameters`, and the OpenAI `{"function": {…}}` wrapper). A match is promoted to a real tool call only if the name is one of the agent's tools and the arguments are a JSON object. Recovered calls go through the pre-execution guards like any other call, and each recovery is logged as `recovered tool call from content`. The assistant agent is left alone, since it may legitimately answer in prose.

**Pre-execution guards.** Four checks run before any command, and each rejection is explained back to the model as the tool response:
- *Verifier:* destructive patterns and out-of-scope targets are blocked, fail-closed.
- *Strategy gate:* a wordlist attack (`hydra`, `medusa`, `ncrack`, `patator` with `-P`/`-L`) is held back if the state already holds a credential or shows no recon yet. Single targeted attempts (`-p one -l user`) always pass. It fires at most once per task, then trusts the agent.
- *Blocked gestures:* a command shape the model kept repeating after being told to stop (see below).
- *Subtask budget:* the per-subtask command cap. Reporting and completion tools still pass, so the agent can finish rather than deadlock.

**Loop recovery.** Two triggers arm a recovery directive. The first fires when recent failed attempts (as judged by the classifier) look alike. The second, classifier-independent, fires when the same *canonical gesture* repeats while no new fact has been learned: here-doc bodies and quoted strings become placeholders, paths keep only directory and extension, numbers become `N`, so `/work/a_final.py --timeout 10` and `/work/b_final.py --timeout 20` are the same gesture. A new fact resets the counter, so a brute-force or sweep that is still covering new ground is never flagged. If the model ignores the directive three turns in a row, the gesture is blocked and the execution monitor hands the case to the adviser.

**Missing-tool grounding.** The shell's "command not found" forms (bash, sh/dash, zsh) are parsed out of raw terminal output, before compaction, and pinned in the state. Nothing is installed automatically; the agent's own observation is simply kept where it can't be missed.

**Execution-context stamping.** Each terminal result carries a note that it ran in the PentAGI sandbox as root, and that only ssh or other remote wrappers reach a target.

**Decision classifier.** Closed-set decisions are offloaded to the sidecar. It currently picks the container image for a flow and labels tool output as success or failure. A high-confidence failure is recorded in the state as a grounded failed attempt. This is advisory, not a hard gate.

**Dynamic few-shot.** If you provide an example file, the K examples whose input overlaps most with the current situation are prepended to the prompt.

```json
[
  {
    "input": "nmap shows 22/tcp open ssh and the task gives credentials student:letmein",
    "output": "Authenticate with the provided credentials first: ssh student@<target>. Do not brute-force."
  }
]
```

## Measuring it

With small models, every configuration change has to be measured, not assumed. [`small-model-eval/`](small-model-eval/) contains the methodology:

- **Metrics, in priority order:** task completion, fact fidelity after compaction (ask about details from sections already summarized), malformed tool-call rate, steps to result, and out-of-scope actions blocked.
- **Scenarios:** one YAML file per target with scope, objective, success criteria and fidelity questions. See [`scenarios/example.yaml`](small-model-eval/scenarios/example.yaml).
- **Finding the real context limit:** run the same scenario with `SMALL_MODEL_CTX_BUDGET_PERCENT` at 25, 40 and 60. Where the success rate drops sharply is the model's usable limit; stay one step below it.
- **Change one variable at a time.**

The classifier sidecar ships with head-to-head benchmarks against a generative model on the same cases:

```bash
cd small-model-eval/classifier-sidecar
./bench_images.sh              # classifier: container-image selection
./bench_qwen_images.sh         # same cases, generative model via Ollama
./bench_success.sh             # classifier: success/failure of tool output
./bench_qwen_success.sh        # same cases, generative model
```

### Findings so far

- **Image selection:** the classifier matched the generative model's accuracy while running on CPU, about 2× faster, and deterministically.
- **Success/failure labelling:** classifier and generative model were equally accurate but made *different* mistakes, which is why the signal is advisory and why a disagreement between them is a useful escalation trigger.
- **Missing tools:** on a 13B model, shown the clean `not found` line in a focused prompt, the model installed the tool itself 66% of the time, and 100% with a one-line nudge. In a real flow of hundreds of turns the same model looped indefinitely. The gap was salience, not capability, which is what missing-tool grounding addresses.

## Repository layout

What this fork adds on top of upstream:

```
backend/pkg/smallmodel/          self-contained logic, each file with unit tests
  budget.go                      token budget
  state.go                       structured state store (keyed by flow + task)
  extract.go                     regex fact extraction (IPs, ports, banners, CVEs, credentials)
  toolsummary.go                 tool-output compaction (nmap XML, head + tail)
  verifier.go                    scope and destructive-command checks
  strategygate.go                premature brute-force gate
  recovery.go                    loop detection, command canonicalization, recovery directives
  missingtool.go                 "command not found" detection
  execcontext.go                 execution-context stamp
  classifier.go                  client for the decision sidecar
  fewshot.go                     similarity-ranked few-shot examples
  toolcallrecovery.go            tool calls recovered from message text
backend/pkg/providers/
  small_model.go                 wiring between the flow provider and the package
  performer.go                   call sites: state block, budget, sanitize → guards → compact
  image_selection.go             classifier-based image choice
backend/pkg/config/config.go     SMALL_MODEL_* settings and the profile defaults
backend/pkg/cast/chain_ast.go    improved orphaned-tool-call repair message
small-model-eval/                methodology, scenario format, classifier sidecar and benchmarks
SMALL_MODEL_TUNING.md            full design notes
```

## Build and test

The backend requires Go 1.26 (see `backend/go.mod`); the Docker build takes care of this for you. To run the tests locally:

```bash
cd backend
go build ./...
go test ./pkg/smallmodel/... ./pkg/config/... ./pkg/cast/... ./pkg/csum/... ./pkg/providers/...
```

## Known gaps and next steps

- **Session and target handles.** The execution-context stamp is a first step. The robust version is first-class session/target handles, with verification that a claimed action actually ran where the model says it did.
- **Task-context credentials.** The strategy gate only knows about credentials that appear in tool output. Feeding credentials given in the task description into the state would make it catch the exact flow that motivated it.
- **More classifier decisions.** Allow/deny decisions are the next closed-set choice to offload.
- **"Varied but stuck" loops.** Many different actions, none working, none repeated. Deliberately not detected yet, to avoid flagging slow but genuine exploration.
- **First-class tools for mechanism gaps,** for example non-interactive SSH authentication handled in code instead of left to the model.

## Credits and license

Built on [PentAGI](https://github.com/vxcontrol/pentagi) by the PentAGI Development Team / VXControl. All of the agent system, UI, and infrastructure is theirs; this fork adds the small-model layer described above. For everything not covered here, the upstream documentation applies.

Licensed under the [MIT License](LICENSE), Copyright (c) 2025 PentAGI Development Team. See [EULA.md](EULA.md) and [NOTICE](NOTICE) for the upstream terms, including the separate terms for VXControl Cloud services.
