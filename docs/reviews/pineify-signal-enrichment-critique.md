# Critique — Pineify Signal Enrichment Plan (vs oracle plan baseline)

**Review of:** `docs/plans/pineify-signal-enrichment-2026-08-15.md`
**Baseline:** generated plan response in `prompt-exports/oracle-plan-2026-08-15-101330-parity-pineify-plan-51d6.md`
**Scope:** correctness/completeness critique only. No recommendation to drop accurate detail; no scope expansion; no plan rewrite.

---

## 1. Implementation-bearing content missing, weakened, or generalized vs the baseline

1. **`PineifySnapshot` struct shape dropped.** Baseline §3.3 pins the struct: `Trend string`, `RSI, ADX float64`, `Events []PineifyEvent{Type,Name,Date}`, `Rating PineifyRating{Action,Score}`. The plan (§3) generalizes to just `Trend`/`RSI`/`ADX`, `Events[]`, `Rating` with no field-level mapping to the actual tool outputs. As written, the field names are invented — it is not established that `get-stock-event-context` returns `{Type,Name,Date}` or that `get-ai-stock-rating` returns `{Action,Score}`. This is the core of step 4/6 and is under-pinned in the plan.

2. **`compositeZ`/`score` scalar emission loses its provenance.** Baseline §3.6 says the scalar = "the per-symbol z". The plan §6 says only "emit scalar `compositeZ`/`score`". Neither document explains **where the per-symbol cohort z comes from inside `SignalLab(symbol string)`** (`service/signal/compute.go:352`). The z is a cross-sectional quantity computed only in `Rank()` (`compute.go:31`) over a cohort; `SignalLab` has no cohort context and `asset` carries no z. The plan must state whether `SignalLab` recomputes the cohort cross-section or reads a carried value — otherwise step 6's scalar emission is underspecified.

3. **The baseline's "safe to depend on Ranking" justification is weakened.** Baseline §8.7.2/§8.7.5 establishes *which* paid fields are never parsed (`raw.*`, `market{isActive,marketId}`, `meta.*`, `cost{state}`, `liquidation{state}`) and therefore cause zero prompt regression. The plan's gaps table (row R1/§8.4) keeps the verdicts but drops the per-field consumption trace, so the "Ranking is faithful and safe to depend on" bottom line rests on evidence the plan no longer carries. Not blocking, but the confidence claim is now assertion rather than traced evidence.

---

## 2. Under-specified seams, unresolved decisions, contradictions, incorrect references, missing dependencies

1. **The reuse dependency does not exist (incorrect reference / missing dependency — highest severity).**
   Plan §9: *"Reuse: `mcp/provider` MCP client framework (no hand-rolled client)."*
   - `mcp/provider/` is the **LLM-provider registry** (`claude.go`, `deepseek.go`, `gemini.go`, `grok.go`, `kimi.go`, `minimax.go`, `openai.go`, `qwen.go`) — AI model API clients, not a Model Context Protocol client.
   - The whole `mcp` package is an **LLM chat client** (`mcp/interface.go` — `AIClient.CallWithMessages`/`CallWithRequest`; `mcp/client.go` — `buildMCPRequestBody`/`parseMCPResponse`, i.e. chat completions), not a JSON-RPC/tool-call MCP client.
   - There is **no reusable MCP-protocol client** in the repo to call Pineify's real MCP server at `https://agents.pineify.app/mcp` (bearer auth, `initialize`/`tools/call`, JSON-RPC, streamable-HTTP/SSE).
   The "no hand-rolled client" guarantee cannot be met with the referenced package. The export's `mcp/provider + mcp/intro` (§3.9) is equally wrong (`mcp/intro/` is docs plus the same LLM client). An actual MCP client must be written or vendored; the plan's reuse assumption is unsupported and should be replaced with an explicit "net-new MCP-protocol client (or vendored dependency)" line in §3.9/§4.

2. **Cancellation is unplumbed and currently false.**
   Plan §7: *"Cancellation honors `Start`'s `ctx.Done()`; in-flight Pineify acquisition aborts on `acquire(ctx)`."* But:
   - `Start(ctx)` calls `s.Ingest()` with **no ctx** (`service.go:151`, immediate call at `:150`, ticker at `:160`).
   - `Ingest()` takes no ctx and `ingestCtx()` returns `context.Background()` (`service.go:143`).
   There is **no cancellable context anywhere in the ingest path** today. The plan's §4 pseudo-code (`s.enrichWithPineify(ctx, assets)`) introduces a `ctx` that has no source. Threading it requires changing `Ingest()` → `Ingest(ctx)` and both call sites in `Start()`, or sourcing a child ctx from `Start`'s — none of which the plan specifies. As written, the §7 cancellation guarantee is false.

3. **Budget arithmetic is internally inconsistent.**
   Plan Background and §4: "~90 calls/3-min interval **@30/min**". Plan §5 reconciles the default to **20/min** → 20×3 = **60**/interval, not 90. The baseline resolves this (60 for the 20/min default, 90 only at the 30/min ceiling, §3.5/§1); the plan leaves two contradictory targets. A reader targeting "~90" would exceed the configured default budget. Also `K≈min(10,budget)` — the unit of `budget` (per-interval vs per-cycle) and the `budget<10` degenerate case are undefined.

4. **`Coverage=="full"` is undefined and may be unreachable.**
   The boost/hard-reject qualifier (plan §6) requires `Coverage=="full"`, but "full" is never defined as a composition of tools. The flow tier (`track-smart-money`) is persistently unavailable (Background), and flow is the low-priority non-blocking tier. If "full" includes flow, `Coverage` can never be `"full"` and the (default-off) qualifier can never fire — silently dead code. If "full" excludes flow, the plan should say so explicitly. This is material to whether step 6's qualifier is functional at all.

5. **"Options Flow context dimension" has nowhere to land (Heatmap).**
   Plan §6 (Heatmap): *"at most an optional 'Options Flow' context dimension."* But the heatmap payload has **no `dimensions[]`** — it emits `bins[]` plus scalars `symbol/marketType/binStep/markPrice` (`compute.go:331-335`). `MarketAnalysis` has no generic extras field (Background), and the formatter is untouched. Where does an "Options Flow" context row actually reach the AI? Unspecified and absent from **both** docs. Either it goes in Signal Lab (which has `dimensions[]`) or it is dropped; as written it is unmappable.

6. **"up to 5 Pineify rows" vs 3 enumerated rows.**
   Plan §6: "free emits 3 → up to **5** Pineify rows: 'Trend…', 'Catalyst…', 'Analyst…'". It lists exactly 3. 3+3=6 ≤ 8; only 3 Pineify rows fit. "5" is a contradiction (the baseline §3.6 makes the same slip). Minor but should be corrected to 3.

7. **Top-K sourcing for the priority tier is unspecified.**
   `enrichWithPineify` runs inside `Ingest()` on the **local** `assets` map (before the snapshot swap at `service.go:108-116`). `Rank()` is computed on-demand by HTTP handlers from `Snapshot()` (`compute.go:31`); the `Service` holds **no stored ranking** (service.go `Service` struct has only `assets/order/lastIngest/ingestErr`). The plan's "its own top-K mappable items from the prior snapshot's ranking" does not say whether enrichment calls `s.Rank()` itself (on the pre-swap snapshot) or reads a cached product. Needs explicit wiring.

8. **Non-US ticker mismatch (`SKHX` vs `SKHY`).**
   Plan §2 exclusion set lists `SKHX`; the Background live-test says `get-stock-research-snapshot` returned `SKHY`. If these are distinct Samsung-class issues, one may be missing from exclusions. Low confidence — verify against the live xyz universe (already tracked in Open question #1).

---

## 3. Details the code disproves, the task does not require, or a simpler design replaces (with correction)

1. **"Reuse `mcp/provider` … (no hand-rolled client)" is disproven by `mcp/provider/`.** See §2.1. Correction: `pineify_client.go` must implement (or vendor) a genuine MCP-protocol client; `mcp`/`mcp/provider` cannot be the transport. This is the one plan detail the code directly contradicts.

2. **`get-technical-analysis-snapshot` bare-ticker handling is correctly carried.** The plan Background correctly supersedes the self-built doc §2.6's `BINANCE:` usage with the bare `ETHUSDT` requirement, and §3/§9 correctly avoid `find-ai-stock-picks`/`find-technical-setups` for universe building (top picks don't align). No change needed; these are accurate and should stay.

3. **Rate-limit default reconciliation is correct** (3→20, clamp `[1,30]`); the plan and baseline agree and both correctly replace the unenforced `PineifyRatePerMinute` (`config.go:70`). Keep.

---

## 4. Requirements/edge cases/architectural problems absent from **both** the export and the plan

1. **MCP protocol lifecycle is ignored.** Calling Pineify is not a bare HTTP POST to a URL. A real MCP server requires `initialize` handshake, tool-list resolution, per-call `tools/call` JSON-RPC, and for streamable-HTTP/SSE a session. The plan models Pineify as "thin wrappers" and budgets ~60–90 calls as if each were an independent request; real MCP may need session setup, which has its own failure/teardown surface (and would be **needed in `pineify_client.go`**, the reuse gap in §2.1). Neither doc addresses protocol/session handling or testing it.

2. **Enrichment wall-time vs the ingest cadence contract.** `Config.Interval` must be "≤ the engine's min scan cadence so the cache is never stale" (`config.go:6-11`). Enrichment runs synchronously inside `Ingest()` before the snapshot swap. ~60 calls with the single `http.Client` 30s timeout (`service.go:40`) can extend `Ingest()` by minutes, delaying `lastIngest`/snapshot swap and letting HTTP products serve stale data past the cadence contract. The plan's "ingest never aborts" guarantees non-failure but not bounded latency. No total wall-clock budget on enrichment is specified. This is an architectural/ownership gap in both docs.

3. **Enrichment one-cycle lag is unexamined.** Because enrichment targets the **prior** snapshot's top-K, the AI consumes the previous cycle's enrichment for the current ranking; the first ingest has no top-K at all. The plan acknowledges the top-K-empty edge but not the systematic lag and whether it is acceptable for a pre-entry confirmation gate that is otherwise sensitive to freshness.

4. **Auth/token handling discipline is only half-specified.** `config.go` notes the token is "never logged", but the plan does not specify redaction/masking for Pineify request/response logging, nor that enriched responses must not be persisted. Minor, but consistent with the AGENTS.md sensitive-info rules and absent from both.

---

## 5. Questions whose answers would materially change the design or implementation order

1. **Does a reusable MCP-protocol client exist, or must `pineify_client.go` implement JSON-RPC/SSE MCP from scratch (or vendor one)?** This determines whether step 3 ("wrappers") is small or large, whether the "no hand-rolled client" promise holds, and whether ordering changes (protocol client becomes a prerequisite for step 3). — Highest-leverage question; §2.1 suggests the answer is "no reusable client exists."
2. **Does `Coverage=="full"` include the flow tier?** With `track-smart-money` unavailable, if "full" includes flow the boost qualifier can never activate; if it excludes flow, the plan should say so. Affects whether step 6's qualifier is functional.
3. **Where does the Heatmap "Options Flow" context dimension actually render, given no `dimensions[]` in heatmap and no generic extras on `MarketAnalysis`?** If nowhere, it should be dropped; if in Signal Lab, the plan should say so.
4. **How is `ctx` threaded into `Ingest()` so the §7 cancellation guarantee holds?** Requires `Ingest(ctx)` + `Start` call-site changes the plan does not specify.
5. **What is the wall-clock budget for enrichment inside `Ingest()` to protect the cadence contract?** Given ~60 calls × up-to-30s timeouts, an unbounded synchronous enrichment can breach the `Interval ≤ engine cadence` invariant.
6. **Where does `compositeZ`/`score` in `SignalLab` come from?** Recomputed cohort cross-section vs carried value — determines `compute.go` scope and correctness.

---

## Bottom line

The plan is directionally sound and its classification/mapping reasoning, bare-ticker correction, and the "Pineify cannot close the heatmap P0" honesty are accurate. But it is not implementation-ready as claimed: the single biggest problem is the **reuse dependency (`mcp/provider`) does not exist** — an MCP-protocol client must be built or vendored, contradicting the plan's "no hand-rolled client" guarantee. Secondary load-bearing gaps are the **unplumbed cancellation ctx**, the **internal 20/min-vs-90-calls budget contradiction**, the **undefined `Coverage=="full"`** that can dead-lock the boost qualifier, and the **unmappable Heatmap "Options Flow" dimension**. Any of these should be resolved (or explicitly deferred) before step 3–6 implementation, and Q1/§2.1 should be settled first as it changes both effort and ordering.
