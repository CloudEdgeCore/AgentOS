# Multi-Agent Research Workflow (Reference Application)

A complete, runnable AgentOS-native implementation of the multi-agent deep
research workflow from `docs/AgentOS_Multi-Agent_Research_Workflow_具体开发方案.md`.
Seven agent roles, dynamic spawn fan-out, Evidence Memory, critic-driven
second-round research, budget governance, citation validation, and
failure-injection recovery — every layer running on the real kernel stack
(gateway, brokered MCP, admission/scheduling/recovery/workflow controllers,
fenced runtime adapter workers), no mocks inside the pipeline.

## Layout

```
agents/                  8 AgentVersion manifests (7 roles; collector is the
                         routing hop described below)
workflow/research-workflow.json   static workflow template + budgets + dynamic limits
runtime/                 the Go runtime implementing all roles behind one
                         agentos.adapter-http/v1 endpoint (protocol.go, roles.go)
tools/webtools/          web.search / web.fetch webhook tool endpoint
                         (offline deterministic corpus, SSRF-guarded fetch)
config/                  deployment configuration examples
scripts/                 bootstrap / publish / run / inject-failure / cleanup
tests/e2e/               end-to-end scenario suite (build tag `integration`)
```

## Architecture

```
                    Control API (agentos workflow create)
                                   │
                     workflow/research-workflow.json
                                   ▼
        ┌────────────── kernel controllers (25ms reconcile) ──────────────┐
        │ admission → scheduling → workflow orchestration → recovery      │
        └──────────────┬──────────────────────────────────────────────────┘
                       │ fenced assignments (lease + fencing token)
                       ▼
      runtime adapter workers ── MCP over HTTP (execution-window registry)
                       │                │
                       ▼                ├── agentos.model.invoke  → Model Gateway → provider
      research runtime (roles.go) ◄─────┤ agentos.memory.put/search → Memory Gateway (Evidence Memory)
                       │                ├── agentos.tool.invoke web.search/fetch → Tool Gateway → webtools
                       └─ agentos.task.spawn ────► WorkflowSpawnService (dynamic steps)
```

### Roles and data flow

1. **planner** decomposes the goal into ≥3 research questions (`rq-*`) and
   spawns one **search** child per question (`spawn:planner` join group).
2. **search** runs corpus queries through `web.search`, persists discovered
   sources to the `sources` Evidence-Memory namespace AND seeds snippet-level
   evidence bundles so round-1 analysis has material before readers finish.
3. **collector** drains every source from its dependency group plus the
   sources namespace, deduplicates by normalized URL, skips sources already
   read (`read-<sourceId>` markers), and spawns one **reader** per unread
   source — capped at 8 per drain (see deviation below), surplus deferred.
4. **reader** fetches the full document via `web.fetch` (SSRF-guarded),
   extracts 2-6 verbatim-evidenced claims via the reader model tier, and
   GROUNDS them before persisting: each claim's evidence must appear verbatim
   (whitespace-normalized) in the fetched source text, else the claim is
   rejected; survivors are stamped with `sourceHash` (sha256 of the source)
   and `grounded:true`, then written to `ev-<sourceId>` + read marker. Zero
   grounded claims is a hard error (the attempt retries).
5. **analyst** synthesizes findings/contradictions/unknowns from evidence,
   citing claim ids; a carried PASS verdict replays the stored analysis.
6. **critic** scores the analysis with THREE terminal states:
   `PASS`; `NEEDS_MORE_RESEARCH` (gaps spawn round-N+1 **search** children
   via `spawn:critic-rN` join groups); or — from round three on, never
   forced into PASS — `INSUFFICIENT_EVIDENCE`.
7. **writer** renders the report strictly from findings+evidence with
   verbatim quotes, declaring `"insufficientEvidence":true` when the final
   verdict demands it; **citation-validator** grades coverage under STRICT
   grounding rules (≥0.90, zero unsupported), requests revisions with
   retry-scoped feedback, and ships an honest verdict after 2 failed
   revisions.

### Provenance chain: Report → Citation → Evidence → Source

Citation validation is as strict as extraction: a citation counts as
supported ONLY when its evidenceId names an existing claim AND its quote is
a non-empty whitespace-normalized substring of THAT claim's evidence.
Empty quotes, unknown claim ids, and passages borrowed from different
evidence are rejected outright — no corpus-wide fallback. Combined with
reader-side grounding, every accepted citation traces to verified source
text end to end.

### Documented deviation: the collector role

The draft plan has search results flow straight into per-source readers.
The kernel's spawn-group join covers DIRECT children of the spawning step
only, so a search wave cannot itself be joined by later steps once it fans
out again. The collector exists as that direct-child boundary: it turns each
finished search wave into ONE bounded, joinable reader wave
(`maxChildrenPerStep=8` respected locally with deferral instead of failing
the step — a failed parent leaves its join group permanently open). This is
the same shape production pipelines use as a "fan-in/fan-out router" step.

### Governance surfaces exercised

- **Budgets**: per-role task budgets in dollars (`costUsd`), workflow cap
  (maxTasks/maxTokens/maxCostUsd), dynamic-spawn ceilings
  (maxDynamicSteps/maxChildrenPerStep/maxSpawnTokens...). Exhaustion stops
  cleanly (`TestResearchWorkflowBudgetStop`).
- **Retries**: multi-attempt tasks requeue for a fresh dispatch on clean
  failure (`retryPolicy.maxAttempts`); lease-expiry recovery places attempt
  N+1 inside the original run; both paths are covered by kernel integration
  tests and the e2e failure scenarios.
- **Fencing**: every tool/model/memory call carries attempt identity +
  fencing token; stale writers are rejected at the gateway.
- **Idempotency**: spawn replays return `replayed`; checkpoints, completions
  and model calls are idempotent-keyed.

## Running locally

```bash
# 1. Infrastructure (Postgres required)
export DATABASE_URL='postgres://agentos:...@127.0.0.1:5432/agentos?sslmode=disable'
export ADAPTER_ENDPOINT='http://127.0.0.1:8090'   # research runtime HTTP endpoint
examples/research-workflow/scripts/bootstrap.sh

# 2. Publish the eight agents
MODEL_REF=openai/gpt-4o-mini \
  examples/research-workflow/scripts/publish-agents.sh

# 3. Run a research workflow end to end
examples/research-workflow/scripts/run-research.sh \
  "Three-year outlook on agent runtime infrastructure"

# Fault injection (crash a worker mid-flight; recovery finishes the work)
examples/research-workflow/scripts/inject-failure.sh crash research-worker-00

# Tear down processes (data preserved)
examples/research-workflow/scripts/cleanup.sh
```

The example runtime (`runtime/cmd`) serves all roles on one endpoint;
`config/runtime-bindings.json` maps each published ref to it.

## Live mode: real models and real web (roadmap P0/P1/P2)

The deterministic stack stays the CI default. Two independent switches
upgrade fidelity without touching the agents — they keep calling the same
logical tiers (`research-fast` / `research-reader` / `research-reasoning`
manifest placeholders) and the same `web.search@1.0.0` / `web.fetch@1.0.0`
tools:

```bash
# P0 — real model (OpenAI-compatible / vLLM / Qwen / DeepSeek / GLM …)
AGENTOS_RESEARCH_LIVE=1
AGENTOS_RESEARCH_MODEL_BASE_URL=https://api.example.com/v1
AGENTOS_RESEARCH_MODEL_KEY=sk-...
AGENTOS_RESEARCH_MODEL_PROVIDER=openai            # registry name, default openai
AGENTOS_RESEARCH_MODEL_FAST=gpt-4o-mini           # wire model per tier (optional)
AGENTOS_RESEARCH_MODEL_READER=gpt-4o-mini
AGENTOS_RESEARCH_MODEL_REASONING=gpt-4o

# P1 — real internet
AGENTOS_RESEARCH_LIVE_WEB=1
AGENTOS_RESEARCH_SEARCH_PROVIDER=doubao           # doubao | brave | bing
AGENTOS_RESEARCH_SEARCH_KEY=...
```

Live backends: `webtools.DoubaoSearch` / `webtools.BraveSearch` /
`webtools.BingSearch` provider
adapters behind one `Backend` interface, and `webtools.LiveFetch` — a
hardened fetcher (DNS resolve/validate/IP-pin/dial with mixed-address
rejection, SSRF policy incl. per-hop redirect re-checks, ≤5 hops, 10 MiB
cap, 20 s timeout, content-type allowlist, HTML→readable-text, final URL +
stable source id). Proxy and alternate TLS dial hooks are disabled at this
boundary so they cannot bypass DNS pinning. The deterministic corpus remains
an in-process fallback.
The Doubao adapter follows the
[official Search Custom API](https://docs.volcengine.com/docs/87772/2272953?lang=zh):
Bearer API-key authentication, the documented `Query` / `SearchType` /
`Count` / `Filter` envelope, URL-bearing web results only, and a client-side
4 QPS limiter below the default 5 QPS quota.

Gated acceptance tests (skip unless their env is present):

| Test | Gate | Asserts |
|---|---|---|
| `TestResearchWorkflowLiveModel` | `AGENTOS_RESEARCH_LIVE=1` | SUCCEEDED, coverage ≥ 0.90, zero unsupported, all evidence grounded; prints §5 metrics JSON |
| `TestResearchWorkflowLiveFull` | + `LIVE_WEB=1`, `…_GOAL="…"` | + grounded rate = 100 %, unique domains ≥ 3, no INSUFFICIENT_EVIDENCE |
| `TestResearchWorkflowLiveRecovery` | same as LiveFull | + kills one active Reader worker, expires/fences its lease, requires a recovered Attempt and final SUCCEEDED |

Metrics (workflowId, questions, sources, uniqueDomains, evidenceCount,
groundedEvidenceRate, citationCoverage, unsupportedCitations, criticRounds,
modelCalls/failures, toolCalls, recoveredAttempts, tokens, costUsd,
duration) are aggregated from the durable store and logged with the run —
identical shape for deterministic and live executions. Every live acceptance
also writes a credential-scanned JSON evidence document containing the exact
commit SHA and all metrics. The default output directory is
`artifacts/research-live/` (generated files are git-ignored); override it with
`AGENTOS_RESEARCH_EVIDENCE_DIR` when a CI job will upload the documents as PR
or release artifacts.

## End-to-end test suite

Requires PostgreSQL at `AGENTOS_TEST_DATABASE_URL`. Each scenario gets its
own schema and a full in-process kernel stack:

```bash
export AGENTOS_TEST_DATABASE_URL='postgres://...'
go test -tags integration -count=1 -timeout 12m \
  -run '^TestResearchWorkflow' ./examples/research-workflow/tests/e2e/
```

| Scenario | Proves |
|---|---|
| `Basic` | goal → ≥3 questions → ≥10 sources → ≥30 claims → report |
| `EvidenceGrounding` | every persisted claim grounded + sourceHash-stamped; nothing ungrounded reaches memory |
| `CriticRetry` | round-1 NEEDS_MORE_RESEARCH spawns gap searches/readers; final PASS |
| `InsufficientEvidence` | round-3 non-PASS becomes INSUFFICIENT_EVIDENCE (never forced PASS); writer+validator declare the shortfall |
| `CitationCoverage` | fabricated quotes trigger writer revision; gate ≥0.90 enforced honestly |
| `InvalidCitation` | persistent unknown-evidence citations never pass the hardened validator; honest best-effort ships |
| `ToolFailureRecovery` | injected `web.fetch` 500s absorbed by attempt retries, no duplicate side effects |
| `ReaderModelRetry` | Reader provider failure creates a failed Attempt and a new Run/Attempt without prematurely writing the read marker |
| `ModelFailure` | provider 429 absorbed by bounded provider retry |
| `Recovery` | SIGKILL-equivalent of both workers mid-run; lease expiry + recovery + restarted instances complete the workflow |
| `BudgetStop` | undersized budget settles instead of running unbounded |
| `LiveModel` / `LiveFull` / `LiveRecovery` (env-gated) | real-model, full live-internet, and live worker-recovery acceptance with durable §5 evidence (see Live mode above) |
| `100Concurrent` (gate `AGENTOS_RESEARCH_SCALE=1`) | 100 simultaneous workflows all reach SUCCEEDED |

Kernel-level regression tests for the retry semantics live in
`internal/kernel/store/postgres/runtime_requeue_integration_test.go`
(requeue on clean failure, exhausted-budget finalization) alongside the
existing checkpoint/recovery tests.

## Configuration reference

See `config/`: tenant policies (`max_priority`, allowed tools/models), model
providers (API keys referenced by env var name only), tool endpoints
(immutable version → HTTPS URL), runtime pools (capacity ledger seeds), and
runtime bindings (published ref → runtime endpoint).
