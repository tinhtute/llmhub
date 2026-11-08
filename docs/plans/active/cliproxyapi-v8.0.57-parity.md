---
id: plan-20261106-v8057
type: plan
intake_id: intake-20261106-v8057
lane: high-risk
status: planned
created: 2026-11-06
updated: 2026-11-06
---

# Plan: CLIProxyAPI v8.0.45..v8.0.57 full parity

> Locked from repository authority. `zharness preflight brainstorm --mode lock --json` is not part of the lifecycle in this
> repository (`AGENTS.md`: the binary is install/update/uninstall only), so the lock is recorded from the checkpoint, ledger,
> gap matrix and the Lead's scope decision instead of a preflight claim.

## Outcome
- result: Port every behaviour-bearing upstream commit in `v8.0.45..v8.0.57` as a semantic port inside the existing llmhub
  seams — translator, auth/credential lifecycle, usage/logging, registry/compression, provider executors, transport pools,
  pluginhost and the new KV store. Scope is full parity for this range, not a targeted subset: the Lead selected all four
  offered slices and named `8.0.57` as the target. Parity is still achieved by semantic ports, never by merging upstream.
- success_signals:
  - Every requirement R1-R39 cites at least one upstream `{tag} {sha}` and lands with a focused package test.
  - No upstream merge commit, no upstream script, no upstream build step, no branding/README port.
  - `internal/pluginhost` stops being a type-alias stub: the plugin ABI lands additively under `sdk/plugin*` with the existing
    `handlers.RequestLifecyclePlugin` seam preserved.
  - The KV store is an additive local cache; Postgres stays the authoritative runtime store and no second source of truth appears.
  - `go build ./...` and `go test ./...` are green at the gate; each phase's focused filter is green at that phase.
  - The gate re-resolves the latest upstream release and records any newer delta as explicit follow-up, never silent scope growth.

## Authority and Requirements
- authority:
  - `docs/upstream/cliproxyapi-checkpoint.json` — checkpoint `v8.0.57` at `c2f402b45643f0740d70b9010fd7d8d0aab7ef65` /
    `refs/upstream-checkpoints/cliproxyapi/v8.0.57`, published `2026-11-05T16:20:39Z`; range `v8.0.45..v8.0.57`, 12 releases;
    scope strategy `targeted-semantic-ports`; include 8 slices, exclude 0. `defer` is hand-maintained (the script has no
    `--defer` flag).
  - `docs/upstream/cliproxyapi-gap-v8.0.45..v8.0.57.json` — 969 changed paths (match 0, baseline 29, upstream-add-absent 75,
    diverged-absent 481, upstream-delete-present-local 0, semantic-review 384). Regenerate with `upstream_gap.py`.
  - `docs/upstream/cliproxyapi-ledger-v8.0.45..v8.0.57.md` — 132 non-merge commits across 12 releases, one row per commit.
  - The Lead's scope decision (2026-11-06): all four offered slices, target `8.0.57`, i.e. the full range.
  - `AGENTS.md` / `docs/WORKFLOW.md` — Postgres-authoritative runtime; no new `web/**/*_test.go`; public SDK changes additive
    only; llmhub branding retained.
  - Local git baseline: `fb556afe8` on `quaqua/llmhub-integ` (equals `origin/quaqua/llmhub-integ`).
- requirements:
  - R1 [accepted]: A Claude `tool_result` with `is_error` keeps its error semantics when translated into Gemini and Antigravity
    ingress requests. Local status: absent for those families — `grep -rn "is_error" internal/translator/gemini` has no hit;
    the only local handling is `internal/translator/claude/openai/responses/claude_openai-responses_request.go:454-457`.
    | source: `v8.0.48 068fe153efa6`, `v8.0.48 81de4b9c48e8`
  - R2 [accepted]: A mid-conversation Claude `tool_addition` block is translated instead of dropped. Local status: partial —
    only the executor path at `internal/runtime/executor/claude_executor.go:1797` and `:2586` recognises it; no translator
    handling. | source: `v8.0.48 12d97cfe196b`
  - R3 [accepted]: Numeric JSON-schema constraints are normalized when converting tool parameters, so a valid numeric
    conjunction is not corrupted. Local status: absent — no normalization symbol under `internal/util/`; only the test comment
    at `internal/util/gemini_schema_refs_test.go:876`. | source: `v8.0.51 ec06c3cb8bac` (#7149)
  - R4 [accepted]: Empty `part` objects are stripped from Gemini and Antigravity `systemInstruction.parts`. Local status:
    absent by symbol (`grep -rn "sanitizeSystemInstruction" internal/` → no hit); the attribution strip at
    `internal/translator/antigravity/claude/antigravity_claude_request.go:191-198` is a different behaviour.
    | source: `v8.0.49 4c46f0aaa575` (#7136), `v8.0.49 f42f58719d4e`
  - R5 [partial]: Gemini tool-search calls carry the `tsc_` item-id prefix in Responses, and outbound Codex requests
    normalize legacy `ts_` and unprefixed `tool_search_call` IDs. Local status: the Codex sanitizer half landed; the Gemini
    translator half has no host — the local Gemini Responses translator has no `tool_search` emitter
    (`internal/translator/gemini/openai/responses/` has no tool-search file).
    | source: `v8.0.47 90c447d3a872`
  - R6 [accepted]: `developer`, omitted and `function` roles are normalized in Gemini ingress translators and in
    responses→chat-completions conversion. Local status: partial — only the Antigravity/Claude request translator handles
    `developer` (`internal/translator/antigravity/claude/antigravity_claude_request.go:191-198`).
    | source: `v8.0.51 00b7677102fa`, `v8.0.52 c888ff72d462`
  - R7 [accepted]: Translator fidelity batch: Claude `tool_result` images nest inside `functionResponse.parts`; excess
    `functionResponse` parts are discarded per turn; thinking content-part arrays are translated; client `tool_search` is
    bridged in chat→responses; replayed web-search citations are dropped when results are missing; safety/filter finish
    reasons map to refusal; shortened gateway MCP tool names stay readable. Local status: absent/partial per item — `tool_search`
    has no hit in `internal/translator/`; the rest must be located per commit during work.
    | source: `v8.0.53 be8643051801`, `v8.0.49 234217d081f1`, `v8.0.56 6044c5d14270`, `v8.0.56 8c9a78518e39`,
    `v8.0.53 9acf81d3a673`, `v8.0.53 24b0454811d2`, `v8.0.50 08437a6742a5`
  - R8 [accepted]: Parallel Responses tool-output batches survive an intervening developer notice. | source: `v8.0.51 3ac384983ea2`
  - R9 [not-applicable]: Cooldown state is written next to the auth file when `auth-dir` is relative, not to the process CWD.
    Local status: the file cooldown store does not exist — `sdk/cliproxy/auth/cooldown_state.go` is only the record type
    (63 lines) and `FileCooldownStateStore` has no hit anywhere; the fork keeps cooldown state in Postgres. The fix has no host.
    | source: `v8.0.48 43068c8642f3`
  - R10 [not-applicable]: A sign-in-required Codex credential stays quarantined across a manual refresh. Local status: the
    `Auth` record has no `RequiresSignIn` or rejected-refresh-token state (`grep RequiresSignIn|RejectedRefreshToken` → no
    hit), so the quarantine the commit fixes does not exist locally. | source: `v8.0.50 a96e640d886a`
  - R11 [not-applicable]: Disabled credentials keep their refresh backoff, automatic refresh is skipped when renewal
    credentials are missing, and a rejected refresh token cannot spin the auto-refresh loop. Local status: the state the fix
    reads does not exist — no `RequiresSignIn`, `RejectedRefreshToken`, `RejectedSIWCRefreshToken` or `authSIWCTokenField`
    symbol; the fix depends on the rejected-token state machine from an earlier upstream release.
    | source: `v8.0.51 7b4c64b00687`, `v8.0.51 95f125ffd04c`, `v8.0.53 e582706557b2`
  - R12 [accepted]: xAI credential filenames carry an identity hash, so two accounts cannot collide. Local status: absent —
    no `identityHash` symbol. | source: `v8.0.50 d65840fb5c86`
  - R13 [accepted]: An Anthropic credit-balance 400 is classified as payment-required, not a generic request fault.
    Local status: absent — no credit-balance classifier. | source: `v8.0.50 d00267c3fe24`
  - R14 [accepted]: Antigravity OAuth requests the `aicode` scope. Local status: absent — no `aicode` hit under
    `internal/auth/`. | source: `v8.0.52 7c627ffbdb4a`
  - R15 [not-applicable]: A targeted OAuth re-login is honoured across all built-in providers, and
    `HasAlternativeTargetForModel` passes context and headers. Local status: the management re-login subsystem does not exist —
    no `relogin`/`re-login` hit under `internal/api/handlers/management/`, no `HasAlternativeTargetForModel` anywhere, and the
    upstream split handler files are absent. | source: `v8.0.53 d38997717dd3`, `v8.0.56 585f97deac9a`
  - R16 [partial]: `count_tokens` 501 and unsupported-capability responses stay availability-neutral; Codex renewal-impossible
    is guided; Antigravity model discovery lives in `sdk/auth` and clones parsed model IDs. Local status: the count_tokens half
    landed; the discovery extraction has no host (`sdk/auth/antigravity_models.go` and `sdk/cliproxy/antigravity_models.go` do
    not exist) and the plugin RPC half depends on the absent pluginhost.
    | source: `v8.0.57 1c5372116735`, `v8.0.48 514d09f746ff`, `v8.0.54 f3762c64de51`, `v8.0.54 f49bd5d91ac9`,
    `v8.0.54 0e02765ccfce`
  - R17 [accepted]: Structured usage is captured for Gemini, Vertex, AI Studio and Antigravity. Local status: absent as a
    structured type — `grep "^type .*Usage" sdk/ internal/` finds only quota-alert payloads and test captures.
    | source: `v8.0.48 bd5ec5310aba`
  - R18 [accepted]: Structured usage is captured for OpenAI/Codex, xAI and Anthropic Messages. Local status: absent, except
    `prompt_tokens_details` in `internal/runtime/executor/kiro_executor.go:2401`.
    | source: `v8.0.48 01551f5f6b26`, `v8.0.48 24840fddf843`, `v8.0.48 89e472f3251e`
  - R19 [accepted]: Cached image/audio and interactions modality tokens are extracted. Local status: absent — no
    `CachedImage`/`CachedAudio` symbol. | source: `v8.0.54 6d7e6a3bb9da`
  - R20 [accepted]: Usage edge cases hold: `output_item.done` tool counts, multiline SSE usage, stream-failure usage,
    multipart image params, provisional zero search counters, and the consumed queue slot is cleared. Local status: absent.
    | source: `v8.0.48 82deffb9ca0d`, `v8.0.48 97dd3a4a2f0c`, `v8.0.57 f2e255f73eda`, `v8.0.48 d5eb4ca1d518`
  - R21 [accepted]: Logging holds: no deferred request retention on Responses WebSockets, no 200-OK body buffering, ws
    disconnect logging is synchronized, request logging finalizes on handler panic, deferred body buffering is capped, Claude
    Fast classified status is preserved, and mid-stream cyber blocks classify as 400. Local status: absent — no `recover()`
    in `internal/api/middleware/`, no deferred-body cap. | source: `v8.0.48 4cb98e3ccdc3`, `v8.0.56 69a032dea368`,
    `v8.0.56 0a94b2aefb52`, `v8.0.56 c3938b72bdaa`, `v8.0.56 be69162478da`, `v8.0.52 a5795047bfb4`
  - R22 [accepted]: TTFT is recorded for direct plugin executor routes, and the TransportCache mutex is released when a
    builder panics. Local status: partial — TTFT exists for Codex
    (`internal/runtime/executor/codex_executor.go:1605`), not for plugin routes. | source: `v8.0.56 f208c90824f2`,
    `v8.0.52 8a82bed5825e`
  - R23 [accepted]: Registry web-search support is indexed, removing the write-lock convoy. Local status: absent — no index
    symbol under `internal/registry/`. | source: `v8.0.46 bc80710469dd`, `v8.0.46 58874f3bf392`
  - R24 [deferred]: Response compression encoders are reused from a pool with a smaller zstd window. Local status: the base
    middleware is absent — `internal/api/response_compression.go` does not exist locally and no `Content-Encoding` writer is
    registered, so this range only optimizes a feature the fork never adopted. Adopting response compression is a separate
    capability decision with its own policy defaults. | source: `v8.0.46 16c6b4bd8c03`, `v8.0.46 a3e76891b1a1`,
    `v8.0.46 12f2cafe33b3`
  - R25 [accepted]: Upstream zstd is decoded synchronously and unlabeled bodies are sniffed lazily. Local status: partial —
    zstd is imported at `internal/runtime/executor/claude_executor.go:22` with magic sniffing at `:1287`.
    | source: `v8.0.46 3c948a580897`
  - R26 [not-applicable]: `selectGroupResponses` is O(N+M) and `systemInstruction` sanitize is deduplicated. Local status: the
    local `fixCLIToolResponse` matches responses FIFO and never had the O(N²) scan
    (`internal/translator/antigravity/gemini/antigravity_gemini_request.go:799-937`), so the optimization has no host; the
    id/name matching upstream added earlier is a separate behaviour, not this range's perf fix.
    | source: `v8.0.49 250d598175db`
  - R27 [deferred]: gjson/sjson/base64 usage migrates to `MoeForge/moego`. Local status: absent, and the upstream commit is
    not a mechanical import swap — it introduces `*ojson.Doc` rewrite pipelines, `FinalizePayloadDoc`, batch edits and
    `strings.Clone` ownership protection across the module. Porting it is an architectural refactor, not a behaviour port.
    | source: `v8.0.52 6e6afab99a19`, `v8.0.54 78a9cc9aaa8a`
  - R28 [accepted]: Codex remote compaction v2 works on openai-compatible routes. Local status: deferred in the prior cycle;
    no v2 support found. | source: `v8.0.55 d57270f465cc`
  - R29 [accepted]: `max_output_tokens` survives request translators, Codex input item IDs are sanitized, and
    `content_token_count` is dropped from replayed items. | source: `v8.0.53 01b24f1f95dc`, `v8.0.54 606e667a8ef1`,
    `v8.0.50 9037d5430db9`
  - R30 [accepted]: Image routing holds: SIWC is bypassed for OpenAI image requests, `gpt-image-2.6` is supported,
    Responses-Lite hosted `image_generation` is compatible, and `supports_image_detail_original` is disabled for non-Codex
    catalog routes. | source: `v8.0.56 dfe8b182c54d`, `v8.0.56 7a7d37ecb5c6`, `v8.0.57 8fb60326f405`,
    `v8.0.53 a1eeb60ec3f7`
  - R31 [accepted]: Responses WebSocket holds: terminal 429 frames are exposed, late interrupts after terminal events are
    rejected, `response.metadata` is dropped for non-Codex clients, private metadata is filtered, and SSE framing recovers.
    | source: `v8.0.56 b5e6027ce4d7`, `v8.0.46 9717e325ba6a`, `v8.0.46 86d951cd5090`, `v8.0.46 ae65e26df2f8`
  - R32 [accepted]: xAI Lite resolution/catalog fallback and Grok Imagine Video 1.5 Lite are supported.
    | source: `v8.0.51 c69cdd933d19`, `v8.0.51 f0ddc38f2921`
  - R33 [accepted]: Devin/AIStudio/Meta parity: model catalog, sealed reasoning items, single-Doc thought step, exec-phrase
    sanitize and no retry on request-caused 403, mid-conversation effort directives, compound fast thinking levels, JSON
    control-character classification, relay SSE frame reassembly, Muse namespace/headers, and
    `X-Tbh-Contributor-Mode` only for contributor models. | source: `v8.0.57 a3c84a37c3a7`, `v8.0.53 b3442f6544c5`,
    `v8.0.53 cfa25b4a0a80`, `v8.0.53 e25b7dafa0ea`, `v8.0.54 061ffe6fbc22`, `v8.0.54 fb30a9e8bb4a`,
    `v8.0.50 ecbaa6c710bb`, `v8.0.48 02ee313962ba`, `v8.0.48 498170a49cc9`, `v8.0.48 7df437c8229e`,
    `v8.0.50 bb6e1aa8c0e8`, `v8.0.50 5d8fddd338df`, `v8.0.57 c2f402b45643`
  - R34 [accepted]: Upstream transports pool per API key: config settings, lazy uTLS allocation, per-runtime isolation,
    pruning, keepalive stamping, credential-dedup keys, and a `http.DefaultTransport` guard. Local status: only the
    Antigravity-specific pool exists — `internal/config/config.go:419-433`. | source: `v8.0.50 34a01b7602b3`,
    `v8.0.50 bf834311e6d6`, `v8.0.50 638f5f55e0d9`, `v8.0.50 6c231c23321b`, `v8.0.50 893958e40169`,
    `v8.0.50 fd02b720e6b3`, `v8.0.52 8643ea56d520`, `v8.0.50 a21814ec608a`
  - R35 [accepted]: Plugins can patch raw request bodies by byte range and can opt out of body delivery.
    Local status: absent — `internal/pluginhost/lifecycle.go` is a type-alias shim over `sdk/api/handlers`.
    | source: `v8.0.47 0fe38224e529`, `v8.0.47 e324e4814c2f`
  - R36 [accepted]: Plugin model registrars publish for multiple providers; plugins register multiple OAuth entries with
    per-entry name/logo; plugin locales are normalized and passed through management. | source: `v8.0.48 bc81923ba63a`,
    `v8.0.49 a8dbebabdf0a`, `v8.0.51 53ff2f9e7478`, `v8.0.51 ce39bc620f65`
  - R37 [accepted]: Plugin runtime safety holds: streams scoped to owning plugin/instance, registration rejected after
    teardown, same-format streams deframed, disabled auth preserved across refresh, dlopen crashes isolated by a preflight
    probe, and hot reload aborts on quiesce failure. | source: `v8.0.52 3698b1597aba`, `v8.0.52 d96b19416846`,
    `v8.0.50 f7331b519773`, `v8.0.50 41c03b74511a`, `v8.0.53 04208efa8b52`, `v8.0.53 dde469d3c2cf`,
    `v8.0.54 6e1035996839`
  - R38 [accepted]: A local bbolt KV store exists and routes integration state, without becoming a second runtime source of
    truth. Local status: absent — no `internal/kvstore`, no `bbolt` in `go.mod`. | source: `v8.0.55 ec14dc4703f0`
  - R39 [accepted]: Cloaked Claude session date pins persist across restarts, and `native-entrypoints` is configurable for
    Claude Code clients. Local status: absent for both. | source: `v8.0.55 e6e5708e677a`, `v8.0.53 6d4102f33f10`
  - R-gate [accepted]: Re-resolve the latest upstream release, refresh the checkpoint, and record any newer delta as explicit
    follow-up. | source: `docs/WORKFLOW.md`, upstream skill Phase 4.

## Non-goals
- NG1: Upstream README/branding commits — `8365de86b6b1`, `b4cbda591510`, `c6425a6e6c7a`, `535110e109e4`, `be2563ff74a4`,
  `b1a3e50bdc33`. Documentation only; llmhub branding is retained.
- NG2: Upstream `AGENTS.md` policy commit `54778bd30de5` (their internal moego rule). Not product behaviour.
- NG3: Test-only churn commits `65889d39f0bd` and `fa42f390c120`. Their behaviour is covered by the owning requirement's check.
- NG4: A wholesale upstream merge, upstream scripts, upstream dependency installs or upstream build steps. Upstream content is
  read-only data.
- NG5: New `web/**/*_test.go` files; the frontend is out of scope.
- NG6: Making the KV store authoritative. Postgres remains the runtime store; `internal/kvstore` is an additive local cache.
- NG7: Breaking public `sdk/**` signatures. Plugin ABI additions are additive; the existing lifecycle seam is preserved.
- NG8: Porting upstream's file splits (e.g. `sdk/cliproxy/service_models.go`, `internal/runtime` package splits). Local layout
  wins; behaviour is ported into the local owner file.

## Scope Policy (this plan)
- source: `docs/upstream/cliproxyapi-checkpoint.json` `scope_policy`, written with
  `python3 .claude/skills/upstream/scripts/upstream_sync.py policy --slug cliproxyapi --strategy targeted-semantic-ports
  --include translator-correctness-v8048-v8056 auth-credential-lifecycle-v8048-v8057 structured-usage-and-logging-v8048-v8056
  perf-registry-compression-v8046-v8052 provider-specific-codex-xai-devin-meta-v8046-v8057
  per-api-key-connection-pools-v8050 plugin-platform-expansion-v8047-v8053
  kvstore-cloak-pins-native-entrypoints-v8055`.
- include (8): the slices above.
- exclude (0): `[]` — the Lead's decision was full parity for this range; nothing is excluded on invariant grounds.
- defer: the prior cycle's defer list is retained in the checkpoint for items outside this range. Items that were deferred and
  now fall inside the range are re-scoped by this plan: `codex-remote-compaction-v2` (R28), `siwc-credential-binding` (R30),
  `codex-auth-refresh` (R10-R11), `claude-cloak-never` (R39), `v8.0.46-v8.0.47-absorption` (R5, R23-R25, R31, R35).
- caveat: `scope_policy.defer` and `exclude = []` are hand-maintained because `upstream_sync.py policy` has no `--defer` flag
  and skips an empty `--exclude` (`upstream_sync.py:243-262`).
- authority order: if this plan and the recorded policy disagree, rerun `policy`; do not edit the plan to match.

## Approach and Risks
- approach: eight implementation phases plus a gate, ordered by blast radius and by shared-file ownership, not by commit count.
  P1/P2/P4 are file-disjoint and run first (translator, auth, registry/compression). P3 then owns usage/logging across the
  executors. P5 edits the same executor and translator files for request/response behaviour, so it runs after P3. P6 owns
  transport/config. P7 and P8 are architectural and run last, in that order, because both touch `sdk/cliproxy` integration
  points. Every phase is independently revertable.
- rejected alternatives:
  - Merging upstream: conflicts across 969 paths, imports the plugin platform wholesale, and inverts the local layout.
  - One phase per release: 12 phases with heavy file overlap, no independent verification value.
  - Porting the plugin platform first: largest risk, no dependency from the correctness slices.
- risks and mitigations:
  - R35-R37 are an ABI build-out, not a port: `sdk/pluginabi`, `sdk/pluginapi`, `sdk/pluginstore` do not exist locally. Mitigation:
    land the ABI additively behind the existing `handlers.RequestLifecyclePlugin` seam, keep the plugin host optional at
    runtime, and gate the phase on `go build ./...` plus a plugin-host smoke test.
  - R27 (moego) touches every JSON call site; 6 local files import gjson. Mitigation: mechanical migration in one commit with
    the existing test suite as the behaviour proof; no semantic edits in the same commit.
  - R38 adds `bbolt` to `go.mod`. Mitigation: additive dependency, cache-only role, explicit NG6.
  - Translator divergence means "already present" cannot be inferred from paths. Mitigation: each task starts from
    `git show <sha>` and cites the local symbol; a task that finds the behaviour already present records the citation and
    closes without an edit.
  - Upstream keeps moving. Mitigation: the gate re-resolves the latest release and pins any newer delta as follow-up.
- recovery: each phase is a separate commit series; revert by phase. The gate is the only place that advances the checkpoint.

## Phases and Verification
- planning_status: planned
- phases:
  - phase_slug: v8057-p1-translator-correctness
    story_id: story-20261106-v8057-p1
    status: done
    risk: medium
    goal: Every translator fidelity fix in the range lands, with Gemini/Antigravity tool-result error semantics preserved end to end.
    upstream_sources:
      - v8.0.48 068fe153efa6, 81de4b9c48e8, 12d97cfe196b
      - v8.0.49 4c46f0aaa575, f42f58719d4e, 234217d081f1
      - v8.0.50 08437a6742a5
      - v8.0.51 ec06c3cb8bac, 3ac384983ea2, 00b7677102fa
      - v8.0.52 c888ff72d462
      - v8.0.53 be8643051801, 9acf81d3a673, 24b0454811d2
      - v8.0.56 6044c5d14270, 8c9a78518e39
      - v8.0.47 90c447d3a872
    depends_on: none
    requirements: R1, R4, R5, R2, R3, R6, R7, R8
    allowed_surfaces:
      - `internal/translator/**` (all families)
      - `internal/util/gemini_schema*.go` and its tests
      - `internal/runtime/executor/helps/codex_input_ids.go` (R5's Codex half)
    avoided_surfaces:
      - `internal/runtime/executor/claude_executor.go` — the executor already recognises `tool_addition` at `:1797`/`:2586`;
        R2 is the translator half only
      - `internal/registry/**` — no model data in this phase
    tasks:
      - task: [R1] Port `is_error` preservation into the Gemini and Antigravity Claude-tool-result translators, per `068fe153efa6` and `81de4b9c48e8`. Landed `4c410729a`.
      - task: [R4] Strip empty `systemInstruction.parts` for Gemini and Antigravity, per `4c46f0aaa575` and `f42f58719d4e`. Landed `1b2ca3f37`.
      - task: [R5] Normalize legacy `ts_` and unprefixed `tool_search_call` IDs on outbound Codex requests, per `90c447d3a872`. Landed `0b3950fa1`; the Gemini emitter half has no local host.
      - task: [R2] Translate mid-conversation `tool_addition` blocks in the translator path, per `12d97cfe196b`. Landed `e17d99509`.
      - task: [R3] Normalize numeric schema constraints in the parameter converter, per `ec06c3cb8bac`; prove a numeric conjunction survives. Landed `4147984d6`.
      - task: [R6] Normalize `developer`/omitted/`function` roles in Gemini ingress and responses→chat, per `00b7677102fa` and `c888ff72d462`. Landed `62a6ed04c`.
      - task: [R7] Port the fidelity batch: nested tool_result images (`ab0ad78a9`), excess `functionResponse` parts (`cfc805b26`), thinking content-part arrays (`4ce21210a`), `tool_search` bridge (`63f4ccd92`), web-search citation drop (`56bf9023b`), safety/filter→refusal (`11cd08840`). MCP name readability (`08437a6742a5`) is not applicable: this fork's `shortenNameIfNeeded` keeps `mcp__` names byte-stable, pinned by `TestConvertClaudeRequestToCodex_ToolChoicePreservesExistingMCPName`; the upstream fix repairs a collapse introduced in `e4bf0f72b` (2026-07-23, before `v8.0.45`), which this fork never adopted.
      - task: [R8] Preserve parallel Responses tool-output batches across developer notices, per `3ac384983ea2`. Landed `3180ebb70`.
    checks:
      - check: `go test ./internal/translator/... ./internal/util/... ./internal/signature/... ./internal/runtime/executor/helps/ -count=1`
      - check: a before/after fixture diff for R1 in both directions (Gemini and Antigravity), not only a passing assertion.

  - phase_slug: v8057-p2-auth-credential-lifecycle
    story_id: story-20261106-v8057-p2
    status: done
    risk: medium
    goal: Credential lifecycle fixes land without changing the Postgres authority or the existing cooldown semantics.
    upstream_sources:
      - v8.0.48 43068c8642f3, 514d09f746ff
      - v8.0.50 a96e640d886a, d65840fb5c86, d00267c3fe24
      - v8.0.51 7b4c64b00687, 95f125ffd04c
      - v8.0.52 7c627ffbdb4a
      - v8.0.53 d38997717dd3, e582706557b2
      - v8.0.54 f3762c64de51, f49bd5d91ac9, 0e02765ccfce
      - v8.0.56 585f97deac9a
      - v8.0.57 1c5372116735
    depends_on: none
    requirements: R12, R13, R14, R16
    allowed_surfaces:
      - `internal/auth/**`, `sdk/cliproxy/auth/**`, `sdk/auth/**`
      - `internal/clienterror/**`
    avoided_surfaces:
      - `internal/store/**` schema — no new tables; Postgres stays authoritative
      - `internal/runtime/executor/**` — request-path behaviour belongs to P5
    tasks:
      - task: [R12] Append an identity hash to xAI credential filenames and adopt pre-hash predecessors on re-login, per `d65840fb5c86`. Landed `aebf9a0c7`.
      - task: [R13] Classify an Anthropic credit-balance 400 as payment-required and map it to a 30-minute payment cooldown, per `d00267c3fe24`. Landed `2fb2d8ea1`.
      - task: [R14] Request the `aicode` OAuth scope for Antigravity, per `7c627ffbdb4a`. Landed `673c6aab0`.
      - task: [R16] Keep `count_tokens` 501 and typed unsupported-capability errors availability-neutral, per `1c5372116735`. Landed `21f193be4`; the discovery-extraction and plugin-RPC halves have no local host.
    checks:
      - check: `go test ./internal/clienterror/ ./sdk/cliproxy/auth/ ./sdk/auth/ ./internal/auth/xai/ ./internal/auth/antigravity/ -count=1`

  - phase_slug: v8057-p3-usage-and-logging
    story_id: story-20261106-v8057-p3
    status: planned
    risk: medium
    goal: Structured usage is captured per provider and the logging defects in the range are closed.
    upstream_sources:
      - v8.0.48 bd5ec5310aba, 01551f5f6b26, 24840fddf843, 89e472f3251e, d5eb4ca1d518, 82deffb9ca0d, 97dd3a4a2f0c, 4cb98e3ccdc3
      - v8.0.54 6d7e6a3bb9da
      - v8.0.56 69a032dea368, 0a94b2aefb52, c3938b72bdaa, be69162478da, f208c90824f2
      - v8.0.52 a5795047bfb4, 8a82bed5825e
      - v8.0.57 f2e255f73eda
    depends_on: none
    requirements: R17-R22
    allowed_surfaces:
      - `internal/runtime/executor/**` (usage observation only)
      - `internal/redisqueue/**`, `sdk/cliproxy/**` (usage manager)
      - `internal/logging/**`, `internal/api/middleware/**`, `internal/clienterror/**`, `sdk/api/handlers/**`
      - `internal/api/server.go` — middleware ordering only (`a5795047bfb4`); compression stays P4's
    avoided_surfaces:
      - `internal/runtime/executor/**` request/response translation — P5 owns that; P3 touches usage observation call sites only
      - `internal/api/server.go` compression — P4 owns it
    tasks:
      - task: [R17] Capture structured Gemini/Vertex/AI Studio/Antigravity usage, per `bd5ec5310aba`. Partial: the
        `gemini_stream_tool_usage.go` counter needs a Vertex streaming tool-call assembler that does not exist locally, so the
        counter is re-expressed on `gjson` inside `helps/` rather than porting a translator helper.
      - task: [R18] Capture structured OpenAI/Codex, xAI and Anthropic usage, per `01551f5f6b26`, `24840fddf843`, `89e472f3251e`.
        `24840fddf843` is partial: its speech and websocket executor halves have no local host (`executeSpeech`,
        `XAIWebsocketsExecutor` absent); the parser and `Detail` halves land.
      - task: [R19] Extract cached image/audio and interactions modality tokens, per `6d7e6a3bb9da`. The upstream diff is
        written against `ojson.Result`, so this is a rewrite on `gjson.Result`, not a cherry-pick.
      - task: [R20] Fix usage edge cases and clear the consumed queue slot, per `82deffb9ca0d`, `97dd3a4a2f0c`, `f2e255f73eda`, `d5eb4ca1d518`.
        `f2e255f73eda` is portable with zero prerequisites and lands first; the local `Manager.run` (`manager.go:349-350`) has the
        same leak.
      - task: [R17-R20 prerequisite] Land `sdk/cliproxy/usage/accounting.go` (`TokenBreakdown`, `TokenAccountingSchemaVersion`,
        `EnsureTokenBreakdownForProvider`, pointer helpers) and `helps.StreamUsageBuffer` before the provider slices: local
        `UsageReporter.once` records only the first stream usage event, so without the merge layer every stream fix is discarded.
      - task: [R21] Fix logging: no deferred retention on Responses WS, no 200-OK body buffering, synchronized ws disconnect logging, panic finalize, capped deferred body, Claude Fast status, cyber-block classification, per `4cb98e3ccdc3`, `69a032dea368`, `0a94b2aefb52`, `c3938b72bdaa`, `be69162478da`, `a5795047bfb4`.
        Re-scoped with evidence: `4cb98e3ccdc3` and `0a94b2aefb52` are not applicable — the deferred-retention subsystem does not exist
        (`grep -rn "DeferredAPIRequest\|deferredBodyCapture" internal/ sdk/` → 0 hits) and `shouldBufferResponseBody` already buffers
        nothing below 400 while `claudeFastRequestError` has no host. `69a032dea368`, `c3938b72bdaa` and `be69162478da` are partial:
        their logging/handler halves land here, their executor halves belong to the provider slices.
      - task: [R22] Release the TransportCache mutex on builder panic, per `8a82bed5825e` (ported in the `w2-pools` slice because
        `transport_cache.go` is also R34's retirement surface). TTFT for direct plugin executor routes (`f208c90824f2`) is
        **moved to P7**: `sdk/api/handlers/handlers_execution.go` does not exist and `ExecutePluginExecutor`/`SetModelRouterHost` have
        0 hits, so P7 must land the plugin route first. This inverts the phase dependency for R22a; record it in P7's entry.
    checks:
      - check: `go test ./internal/runtime/executor/... ./internal/redisqueue/... ./internal/logging/... ./internal/api/middleware/... -count=1`
      - check: a fixture that proves the panic path finalizes the request log (it must fail before the fix).

  - phase_slug: v8057-p4-perf-registry-compression
    story_id: story-20261106-v8057-p4
    status: done
    risk: low
    goal: Registry reads stop convoying on the write lock and upstream bodies decode without blocking the caller.
    upstream_sources:
      - v8.0.46 bc80710469dd, 58874f3bf392, 16c6b4bd8c03, a3e76891b1a1, 12f2cafe33b3, 3c948a580897
      - v8.0.49 250d598175db
      - v8.0.52 6e6afab99a19
      - v8.0.54 78a9cc9aaa8a
    depends_on: none
    requirements: R23, R25
    allowed_surfaces:
      - `internal/registry/**`
      - `internal/runtime/**` zstd decode helper
    avoided_surfaces:
      - `internal/api/**` compression — R24 deferred: the base middleware is absent locally
      - `internal/translator/antigravity/**` — R26 not applicable: the local FIFO matcher never had the scan
      - `go.mod`/`go.sum` — R27 deferred: the moego migration is an architectural refactor
    tasks:
      - task: [R23] Index web-search support so reads do not take the registry write lock, per `bc80710469dd` and `58874f3bf392`. Landed `a2f1f34d4`.
      - task: [R25] Decode upstream zstd synchronously and sniff unlabeled bodies lazily, per `3c948a580897`. Landed `30bb3c2f8`; the local helper is inline in `internal/runtime/executor/claude_executor.go`, not `helps/response_decode.go`.
    checks:
      - check: `go test ./internal/registry/ -count=1` and `go test ./internal/runtime/executor/ -count=1`

  - phase_slug: v8057-p5-provider-specific
    story_id: story-20261106-v8057-p5
    status: planned
    risk: medium
    goal: Codex, xAI, Devin, AI Studio and Meta request/response behaviour matches the range.
    upstream_sources:
      - v8.0.46 9717e325ba6a, 86d951cd5090, ae65e26df2f8
      - v8.0.48 02ee313962ba, 498170a49cc9, 7df437c8229e
      - v8.0.50 ecbaa6c710bb, 9037d5430db9, bb6e1aa8c0e8, 5d8fddd338df
      - v8.0.51 c69cdd933d19, f0ddc38f2921
      - v8.0.53 01b24f1f95dc, b3442f6544c5, cfa25b4a0a80, e25b7dafa0ea, a1eeb60ec3f7
      - v8.0.54 606e667a8ef1, 061ffe6fbc22, fb30a9e8bb4a
      - v8.0.55 d57270f465cc
      - v8.0.56 dfe8b182c54d, 7a7d37ecb5c6, b5e6027ce4d7
      - v8.0.57 8fb60326f405, a3c84a37c3a7, c2f402b45643
    depends_on: v8057-p3-usage-and-logging
    requirements: R28-R33
    allowed_surfaces:
      - `internal/runtime/executor/**` (Codex, xAI, Devin, AI Studio)
      - `internal/translator/{codex,openai,gemini,antigravity}/**`
      - `internal/registry/**` model data for image/devin catalogs
      - `internal/client/**` capability flags
    avoided_surfaces:
      - usage observation call sites — P3 owns them
      - `internal/pluginhost/**` — P7
    tasks:
      - task: [R28] Support Codex remote compaction v2 on openai-compatible routes, per `d57270f465cc`.
      - task: [R29] Preserve `max_output_tokens`, sanitize Codex input item IDs, drop `content_token_count` on replay, per `01b24f1f95dc`, `606e667a8ef1`, `9037d5430db9`.
      - task: [R30] Bypass SIWC for image requests, add `gpt-image-2.6`, support Responses-Lite hosted `image_generation`, disable `supports_image_detail_original` for non-Codex routes, per `dfe8b182c54d`, `7a7d37ecb5c6`, `8fb60326f405`, `a1eeb60ec3f7`.
      - task: [R31] Expose terminal 429 frames, reject late ws interrupts, drop `response.metadata` for non-Codex, filter private metadata, recover SSE framing, per `b5e6027ce4d7`, `9717e325ba6a`, `86d951cd5090`, `ae65e26df2f8`.
      - task: [R32] Support xAI Lite resolution/catalog fallback and Grok Imagine Video 1.5 Lite, per `c69cdd933d19` and `f0ddc38f2921`.
      - task: [R33] Port the Devin/AI Studio/Meta batch listed in R33.
    checks:
      - check: `go test ./internal/runtime/executor/... ./internal/translator/... ./internal/client/... -count=1`
      - check: `go test ./internal/registry/... -count=1` for the catalog additions.

  - phase_slug: v8057-p6-connection-pools
    story_id: story-20261106-v8057-p6
    status: planned
    risk: medium
    goal: Upstream transports pool per API key with a safe default and no cross-runtime leakage.
    upstream_sources:
      - v8.0.50 34a01b7602b3, bf834311e6d6, 638f5f55e0d9, 6c231c23321b, 893958e40169, fd02b720e6b3, a21814ec608a
      - v8.0.52 8643ea56d520
    depends_on: none
    requirements: R34
    allowed_surfaces:
      - `internal/config/**` (new per-key pool section; keep `AntigravityConnectionPoolConfig` intact)
      - `internal/runtime/**` transport cache
      - `internal/watcher/**` config diff
      - `sdk/cliproxy/**` credential dedup keys
    avoided_surfaces:
      - `internal/runtime/executor/**` request logic
      - `internal/api/**` compression — P4
    tasks:
      - task: [R34] Add per-api-key connection-pool config and pool upstream transports per key, per `34a01b7602b3` and `bf834311e6d6`.
        Partial: local has no v8 `api-keys:` group schema (`config_v8.go`, `sharedKeyFields`, `expandV8Groups` absent), so group
        defaults exist only for `OpenAICompatibility`; the YAML flattening hunks are not-applicable.
      - task: [R34] Honour per-key opt-in, isolate pools per runtime, lazily allocate uTLS pools, and close fenced/retired transports outside the request path, per `638f5f55e0d9`, `6c231c23321b`, `fd02b720e6b3`.
        Partial: no Claude-Code uTLS transport exists locally (`cachedClaudeCodeRoundTripper`, `IsAnthropicUpstreamURL` absent) and
        `NewUtlsHTTPClient` has no ctx parameter, so the Claude-Code pool kind and the pluginhost/compat rebind are not-applicable.
      - task: [R34] Decline pooling when `http.DefaultTransport` is not `*http.Transport`, and include pool settings in credential dedup keys, per `893958e40169` and `8643ea56d520`.
        Land order: `bf834311e6d6` → `34a01b7602b3` → `638f5f55e0d9` → `6c231c23321b` → `893958e40169` → `fd02b720e6b3` →
        `8a82bed5825e` → `8643ea56d520` → `a21814ec608a`. `8a82bed5825e` (R22b) belongs here because `transport_cache.go` is
        P6's retirement surface; its `Purge` half depends on `fd02b720e6b3`'s `retired` flag.
    checks:
      - check: `go test ./internal/config/... ./internal/runtime/... ./internal/watcher/... -count=1`
      - check: a test that a non-`*http.Transport` default declines pooling instead of panicking.

  - phase_slug: v8057-p7-plugin-platform
    story_id: story-20261106-v8057-p7
    status: planned
    risk: high
    goal: The plugin platform becomes real — ABI, multi-provider models, multi-entry OAuth, stream scoping and crash isolation — without breaking the existing lifecycle seam.
    upstream_sources:
      - v8.0.47 0fe38224e529, e324e4814c2f
      - v8.0.48 bc81923ba63a
      - v8.0.49 a8dbebabdf0a
      - v8.0.50 f7331b519773, 41c03b74511a
      - v8.0.51 53ff2f9e7478, ce39bc620f65
      - v8.0.52 3698b1597aba, d96b19416846
      - v8.0.53 04208efa8b52, dde469d3c2cf
      - v8.0.54 6e1035996839
    depends_on: v8057-p3-usage-and-logging
    requirements: R35-R37
    allowed_surfaces:
      - `internal/pluginhost/**` (replacing the type-alias shim at `internal/pluginhost/lifecycle.go:1-15`)
      - new `sdk/pluginabi/**`, `sdk/pluginapi/**`, `sdk/pluginstore/**`, `internal/pluginstore/**`
      - `internal/api/**` plugin endpoints
      - `sdk/cliproxy/**` registration and teardown
    avoided_surfaces:
      - `internal/store/**` — no plugin state in Postgres
      - `internal/runtime/executor/**` — plugin execution goes through the existing executor seam
    tasks:
      - task: [R35] Add raw request-body byte-range patching and let frontend auth providers opt out of body delivery, per `0fe38224e529` and `e324e4814c2f`.
      - task: [R36] Allow multi-provider model registration, multi-entry OAuth with per-entry name/logo, and plugin locale normalization with management passthrough, per `bc81923ba63a`, `a8dbebabdf0a`, `53ff2f9e7478`, `ce39bc620f65`.
      - task: [R37] Scope model execution streams to the owning plugin/instance, reject registration after teardown, deframe same-format streams, preserve disabled auth across refresh, isolate dlopen crashes with a preflight probe, and abort hot reload on quiesce failure, per `3698b1597aba`, `d96b19416846`, `f7331b519773`, `41c03b74511a`, `04208efa8b52`, `dde469d3c2cf`, `6e1035996839`.
    checks:
      - check: `go build ./...` and `go test ./internal/pluginhost/... ./sdk/plugin... -count=1`
      - check: a plugin-host smoke test that loads a built-in plugin, registers a model for two providers, and survives a teardown race.

  - phase_slug: v8057-p8-kvstore-and-entrypoints
    story_id: story-20261106-v8057-p8
    status: planned
    risk: medium
    goal: A local bbolt KV store backs cloaked session date pins, and native entrypoints are configurable.
    upstream_sources:
      - v8.0.55 ec14dc4703f0, e6e5708e677a
      - v8.0.53 6d4102f33f10
    depends_on: v8057-p7-plugin-platform
    requirements: R38-R39
    allowed_surfaces:
      - new `internal/kvstore/**`
      - `internal/config/**` (kvstore + native-entrypoints sections)
      - `internal/runtime/**` integration routing
      - `sdk/cliproxy/**` wiring
      - `go.mod`/`go.sum` for bbolt
    avoided_surfaces:
      - `internal/store/**` — Postgres remains authoritative (NG6)
      - `internal/pluginhost/**` — P7 owns it
    tasks:
      - task: [R38] Add the local bbolt KV store and integration routing, per `ec14dc4703f0`; cache-only role, no authoritative reads.
      - task: [R39] Persist cloaked session date pins in the KV store and add `native-entrypoints` config for Claude Code clients, per `e6e5708e677a` and `6d4102f33f10`.
    checks:
      - check: `go test ./internal/kvstore/... ./internal/config/... ./internal/runtime/... -count=1`
      - check: a restart test proving a date pin survives process restart, and a config test proving `native-entrypoints` parses.

  - phase_slug: v8057-p9-gate
    story_id: story-20261106-v8057-p9
    status: planned
    risk: low
    goal: Prove the range is closed and pin any newer upstream delta as follow-up.
    depends_on: v8057-p1-translator-correctness, v8057-p2-auth-credential-lifecycle, v8057-p3-usage-and-logging, v8057-p4-perf-registry-compression, v8057-p5-provider-specific, v8057-p6-connection-pools, v8057-p7-plugin-platform, v8057-p8-kvstore-and-entrypoints
    requirements: R-gate
    allowed_surfaces:
      - `docs/upstream/**`, this plan
    avoided_surfaces:
      - source code — the gate fixes nothing
    tasks:
      - task: [R-gate] Run `go build ./...` and `go test ./... -count=1` on the merged tree.
      - task: [R-gate] Re-run `upstream_sync.py sync --slug cliproxyapi`; if a release newer than `v8.0.57` exists, record it as explicit follow-up in this plan, not as silent scope.
      - task: [R-gate] Audit that every R1-R39 has a merged commit and a passing check; list any that do not.
    checks:
      - check: `go build ./... && go test ./... -count=1`
      - check: `python3 .claude/skills/upstream/scripts/upstream_sync.py sync --slug cliproxyapi`

### Phase map (one row per phase, with the shared-file note)

| Phase | Requirements | Depends on | Shared-file note |
| --- | --- | --- | --- |
| `v8057-p1-translator-correctness` | R1-R8 | none | sole owner of `internal/translator/**` and `internal/util/gemini_schema*` |
| `v8057-p2-auth-credential-lifecycle` | R12, R13, R14, R16 | none | sole owner of `internal/auth/**`, `sdk/cliproxy/auth/**`, `sdk/auth/**`; R9/R10/R11/R15 re-scoped with evidence |
| `v8057-p3-usage-and-logging` | R17-R22 | none | sole owner of usage observation sites, `internal/logging/**`, `internal/api/middleware/**` |
| `v8057-p4-perf-registry-compression` | R23, R25 | none | sole owner of `internal/registry/**` and the zstd decode helper; R24/R26/R27 re-scoped with evidence |
| `v8057-p5-provider-specific` | R28-R33 | P3 | edits executor/translator files P3 touched for usage only; P3 must merge first |
| `v8057-p6-connection-pools` | R34 | none | sole owner of per-key pool config and transport cache; `internal/watcher/**` after P2 |
| `v8057-p7-plugin-platform` | R35-R37 | P3 | sole owner of `internal/pluginhost/**` and `sdk/plugin*`; `sdk/cliproxy/**` after P3 |
| `v8057-p8-kvstore-and-entrypoints` | R38-R39 | P7 | sole owner of `internal/kvstore/**`; `sdk/cliproxy/**` after P7 |
| `v8057-p9-gate` | R-gate | P1-P8 | docs/upstream only |

## Parallel Execution Directive
- Wave 1: `P1`, `P2`, `P4` — file-disjoint (translator/util; auth/watcher/sdk-auth; registry/api-compression/runtime-zstd).
- Wave 2: `P3`, `P6` — disjoint by file: P3 owns usage observation and logging, P6 owns transport/config. Both compile into
  `internal/runtime`, so "`./internal/runtime/...` green" is only meaningful once both merge.
- Wave 3: `P5` — after P3, because it edits the same executor and translator files for request/response behaviour.
- Wave 4: `P7` — after P3, because plugin execution reuses the usage/TTFT seam.
- Wave 5: `P8` — after P7, because both touch `sdk/cliproxy` integration points.
- Wave 6: `P9` — gate after every phase merges.
- Ordering inside a wave is a risk preference, not a dependency: start with the lowest blast radius (`P4`, then `P2`, then `P1`).

## Wave 2 Standing Orders
Every worker in this wave gets these rules pasted into its brief; a retry gets the same text plus the prior report.

- Port semantically; never merge, rebase onto, or cherry-pick upstream. Cite the upstream short SHA in the code comment and
  in the test that pins the behaviour.
- Local stack is `tidwall/gjson` + `tidwall/sjson`. Never introduce `moego/ojson`, `*ojson.Doc`, or another JSON dependency.
- Public `sdk/**` signatures stay additive; no breaking change to `sdk/cliproxy/**`, `sdk/api/**`, `sdk/proxyutil/**`.
- No new `web/**/*_test.go`.
- Go 1.26 style: `for i := range n` when the body does not need a non-zero start.
- `gofmt -w` every touched file, then `go build ./...` in your worktree before reporting.
- Never edit outside your SCOPE. Never merge, rebase, force-push, or pass `--no-verify`. Never stage another worker's files.
- A report is not evidence: name the exact commands you ran and paste their output.
- A regression test must fail on the pre-fix tree; state how you verified that (run it before the edit, or stash the fix).
- Commit per requirement with a conventional message and the upstream short SHA in the body. Never push.
- Report `PASS | ISSUES | BLOCKED` with branch, commit SHA, commands and output, and every deviation from SCOPE.

## Wave 2 Slices
Shape: **partition**. One owner per file; a file listed in two write sets is a defect in this table, not a negotiation.

| Slice | Phase / reqs | Write set | Depends on | Branch | Acceptance |
| --- | --- | --- | --- | --- | --- |
| `w2-usage-core` | P3 R17-R20 core | `sdk/cliproxy/usage/**`, `internal/runtime/executor/helps/usage_helpers.go`, `helps/plugin_executor_usage.go` (new), `helps/openai_stream_usage_observer.go` (new), `helps/gemini_stream_tool_usage.go` (new), `internal/redisqueue/plugin.go` | none | `wave2/usage-core` | `go test ./sdk/cliproxy/usage/ ./internal/redisqueue/ ./internal/runtime/executor/helps/ -count=1` |
| `w2-logging` | P3 R21, R22a | `internal/logging/**`, `internal/api/middleware/**`, `internal/clienterror/**`, `sdk/api/handlers/**`, `internal/api/server.go`, `helps/logging_helpers.go` | none | `wave2/logging` | `go test ./internal/logging/ ./internal/api/middleware/ ./internal/clienterror/ ./sdk/api/handlers/... -count=1` |
| `w2-pools` | P6 R34, P3 R22b | `internal/config/**`, `internal/watcher/**`, `sdk/cliproxy/**` except `usage/**`, `sdk/proxyutil/proxy.go`, `helps/proxy_helpers.go`, `helps/utls_client.go`, `helps/transport_cache.go`, `internal/api/handlers/management/**`, `internal/pluginhost/**` | none | `wave2/pools` | `go test ./internal/config/ ./internal/watcher/... ./internal/runtime/executor/helps/ ./sdk/cliproxy/ ./sdk/proxyutil/ -count=1` |
| `w2-gemini-family` | P3 R17 + R21 executor hunks | `internal/runtime/executor/gemini_executor.go`, `gemini_vertex_executor.go`, `aistudio_executor.go`, `antigravity_executor.go`, `internal/translator/antigravity/gemini/antigravity_gemini_response.go` | `w2-usage-core` | `wave2/gemini-family` | `go test ./internal/runtime/executor/ ./internal/translator/antigravity/gemini/ -count=1` |
| `w2-openai-codex` | P3 R18/R19/R20 + R21 executor hunks | `internal/runtime/executor/codex_executor.go`, `codex_openai_images.go`, `codex_websockets_executor.go`, `codex_websockets_duplex.go`, `openai_compat_executor.go` | `w2-usage-core` | `wave2/openai-codex` | `go test ./internal/runtime/executor/ -run 'Codex\|OpenAI\|Compat\|Images' -count=1` |
| `w2-xai-meta` | P3 R18 xAI + R20 meta | `internal/runtime/executor/xai_executor.go`, `meta_executor_execute.go`, `meta_executor_stream.go` | `w2-usage-core` | `wave2/xai-meta` | `go test ./internal/runtime/executor/ -run 'Xai\|XAI\|Meta' -count=1` |
| `w2-claude` | P3 R18 Anthropic + R21 claude hunks | `internal/runtime/executor/claude_executor.go`, `claude_executor_diagnostics.go` | `w2-usage-core` | `wave2/claude` | `go test ./internal/runtime/executor/ -run 'Claude' -count=1` |

Held by the main agent after the merges (no worker may touch them):
- `internal/runtime/executor/openai_compat_executor.go` — **no longer held**: the `fd02b720e6b3` `UsesConfig`/`shouldKeepExisting`
  hunks have no local host (local always registers the executor), so the P3/P6 collision disappears.
- `config.example.yaml` — absent locally, so `638f5f55e0d9`'s example-file hunk is not-applicable.
- `internal/pluginhost/**` and `sdk/proxyutil/proxy.go` — the rebind and `socksForwardDialer` hunks have no local host;
  `w2-pools` records them as not-applicable with evidence.

## Validation
- Run each phase's focused filter at that phase, not only at the gate.
- R1, R4, R7 and R31 need real output comparisons (fixture before/after), not only mocked expectations.
- R21's panic-path test must fail on the pre-fix tree; a test that passes before the change is not a regression test.
- R27 is mechanical: the existing suite is the proof; no semantic edits in the same commit.
- R35-R37 need a real plugin-host smoke run, not only compile success.
- All ported requirements stay traceable to upstream commits by short SHA.
- No upstream scripts, installs or build steps are executed; upstream content is read-only data.
- No new `web/**/*_test.go` files.

## Handoff
- Next steps: start Wave 1 (`P1`, `P2`, `P4`). `P4` is the cheapest proof that the branch and the filters work; `P2` is the
  highest-value correctness slice; `P1` is the largest but the most bounded.
- Decomposition: this file is plan-ready; `work` executes one phase per unit. `to-plan` is not required for the phase list.
- Deliberately not taken (so a later reader does not think they were missed): one phase per release, merging upstream, porting
  the plugin platform first, and folding R27 into another phase.
- Active-plan hygiene (separate action, not done here): `docs/plans/active/cliproxyapi-v8.0.33-targeted-parity.md` and
  `cliproxyapi-v8.0.40-targeted-parity.md` are `status: implemented`; the v8.0.45 plan's six phases are merged on
  `quaqua/llmhub-integ` (`4e15ed889`, `c4e80ef66`, `cbbd59ada`, `a2457c11d`, `abac5fe3f`, `1f9389e10`) but its file still says
  `planned`. Archiving them requires their gate evidence; do not move them as part of this plan.

## Log
- 2026-11-08 wave-2 frame: shape **partition**; done predicate = `go build ./...` plus the P3 filter
  (`./internal/runtime/executor/... ./internal/redisqueue/... ./internal/logging/... ./internal/api/middleware/...`) and the P6
  filter (`./internal/config/... ./internal/runtime/... ./internal/watcher/...`) green on the combined tree.
- 2026-11-08 analysis verdicts (read-only scouts, patches at `/tmp/wave2-analysis/patches/`):
  - usage core: the five usage commits are one atomic stack; local has no `StreamUsageBuffer` and `UsageReporter.once` keeps the
    FIRST stream usage event, so the merge layer is a prerequisite. `f2e255f73eda` is portable with zero prerequisites.
  - providers: every family commit needs the core first; upstream split files re-home to `codex_executor.go`,
    `antigravity_executor.go`, `claude_executor.go`, `xai_executor.go`, `codex_websockets_executor.go`; `executeSpeech`,
    `XAIWebsocketsExecutor`, `executeDirectOpenAIImage` and `VertexStreamingToolCall` have no local host.
  - logging: `4cb98e3ccdc3` and `0a94b2aefb52` not-applicable; `69a032dea368`/`c3938b72bdaa`/`be69162478da` partial (logging and
    handler halves only); `a5795047bfb4` portable; `f208c90824f2` (R22a) moves to P7.
  - `d5eb4ca1d518`'s translator half is perf-only and local behaviour already matches: deferred, not a parity gap.
- 2026-11-08 spawns, stage 1 (model `mistral/deepseek-v4-1-flash-1017:max`; per-call models enabled for this session):
  - `w2-usage-core` — branch `wave2/usage-core`, worktree `.worktrees/wave2-usage-core`, job `Wave2UsageCore`.
  - `w2-logging` — branch `wave2/logging`, worktree `.worktrees/wave2-logging`, job `Wave2Logging`.
  - `w2-pools` — branch `wave2/pools`, worktree `.worktrees/wave2-pools`, job `UsageCore-3` (job name is stale, work is R34).
- 2026-11-08 baseline at `71d6255b1`: both phase filters are green before any wave-2 edit
  (`go test ./internal/runtime/executor/... ./internal/redisqueue/... ./internal/logging/... ./internal/api/middleware/ -count=1`
  and `go test ./internal/config/... ./internal/runtime/... ./internal/watcher/... -count=1`).
- 2026-11-08 environment note: sub-agents have no configured model by default (`task.perCallModel: off` plus no agent model), so
  every dispatch must pass `model` explicitly; only `mistral/deepseek-v4-1-flash-1017:max` resolved. A cross-family verifier is
  therefore unavailable and the check lane must be judged by the Lead.

## Progress
- 2026-11-06: `v8057-p4-perf-registry-compression` done. R23 landed `a2f1f34d4` (registry web-search index; 3 new tests).
  R25 landed `30bb3c2f8` (single-concurrency zstd decoder, lazy magic-byte sniff; 1 new test). R24 deferred: the response
  compression middleware does not exist locally, so the range only optimizes a feature the fork never adopted. R26
  not-applicable: the local `fixCLIToolResponse` matches responses FIFO and never had the O(N²) scan. R27 deferred: the moego
  commit is an architectural refactor (`*ojson.Doc` pipelines, `strings.Clone` ownership), not a mechanical import swap.
  Verification: `go build ./...`, `go test ./internal/registry/ -count=1`, `go test ./internal/runtime/executor/ -count=1`.
- 2026-11-06: `v8057-p2-auth-credential-lifecycle` done. R14 landed `673c6aab0`, R13 `2fb2d8ea1`, R12 `aebf9a0c7`, R16
  `21f193be4`. R9/R10/R11/R15 are not applicable: the file cooldown store, the `RequiresSignIn`/rejected-token state and the
  management re-login subsystem do not exist in this fork. Verification: `go test ./internal/clienterror/
  ./sdk/cliproxy/auth/ ./sdk/auth/ ./internal/auth/xai/ ./internal/auth/antigravity/ -count=1`.

- 2026-11-06: `v8057-p1-translator-correctness` in progress. R1 landed `4c410729a` (Gemini + Antigravity tool-result
  `is_error` → `response.error`; both entrypoints and both directions tested). R4 landed `1b2ca3f37` (empty
  `systemInstruction.parts` stripped; 5 cases). R5 landed `0b3950fa1` (Codex `tool_search_call` IDs normalized to `tsc_`,
  collision-safe); the Gemini emitter half has no local host. R3 landed `4147984d6` (numeric schema constraints normalized
  with an in-place edit list; 6 cases). R6 landed `62a6ed04c` (Gemini ingress role inference in Claude/Codex/OpenAI
  translators, snake_case part helpers, no stray empty turn; developer roles preserved in responses→chat). R8 landed
  `3180ebb70` (parallel tool outputs collected across developer/system notices, non-contiguous end index; 4 cases).
  R2 and R7 landed 2026-11-08 (see the next entry).
- 2026-11-08: `v8057-p1-translator-correctness` done. R2 landed `e17d99509` (mid-conversation `tool_addition` in
  codex/claude and openai/claude, inline definitions in the shared Claude tool-name maps; deferred `additional_tools`
  keeps `function_call`/`function_call_output` adjacency). R7 landed `ab0ad78a9` (nested tool_result images), `cfc805b26`
  (excess `functionResponse` parts matched by id/name/order), `4ce21210a` (thinking content-part arrays), `63f4ccd92`
  (`tool_search` bridge), `56bf9023b` (web-search citation drop), `11cd08840` (safety/filter→refusal). R7g
  (`08437a6742a5`) is not applicable: local `shortenNameIfNeeded` keeps `mcp__` names byte-stable by design, and the
  upstream collapse it repairs came from `e4bf0f72b`, which predates `v8.0.45` and was never adopted here. R7e's executor
  half is not portable: no `claudeWebSearchResultCacheScope` or restore hook exists locally. Verification:
  `go build ./...`, `go test ./internal/translator/... ./internal/util/... ./internal/signature/...
  ./internal/runtime/executor/helps/ -count=1`.

## Current State and Next Action
- state: P1, P2 and P4 done. Re-scoped with evidence: R9-R11, R15, R24, R26, R27 not applicable/deferred, R7g not
  applicable. Checkpoint `v8.0.57` at `c2f402b45`; local baseline `fb556afe8` on `quaqua/llmhub-integ`.
- next action: `work` — Wave 2 starts with `P3` (`v8057-p3-usage-and-logging`) and `P6` (`v8057-p6-connection-pools`).
