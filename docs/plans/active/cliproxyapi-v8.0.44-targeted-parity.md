---
id: plan-20261028-v8044
type: plan
intake_id: intake-20261028-v8044
lane: high-risk
status: planned
created: 2026-10-28
updated: 2026-10-28
---

# Plan: CLIProxyAPI v8.0.44 targeted parity

> Locked from repository authority. `zharness preflight brainstorm --mode lock --json` is not part of the lifecycle in this
> repository (`AGENTS.md`: the binary is install/update/uninstall only), so the lock is recorded from the checkpoint, ledger,
> semantic review and the Lead's scope decision (`~/quaqua/runs/tieude-up8044.md`) instead of a preflight claim.

## Outcome
- result: Port the approved v8.0.40..v8.0.44 capability slices as focused semantic ports inside the existing llmhub signature,
  translator, executor, watcher, management and catalog interfaces. Scope stays additive and behaviour-focused: no wholesale
  upstream merge, no branding churn, no pluginhost runtime import, no file/YAML runtime source of truth, no interactions
  translator family.
- success_signals:
  - Every accepted requirement cites at least one upstream `{tag} {sha}` and lands with package-level tests.
  - Postgres remains the authoritative runtime store; management and auth writes continue to persist to it.
  - Amp/Kiro/Gemini CLI/Codex/Claude route behaviour is unchanged except where a requirement says otherwise; the local xAI
    executor stays monolithic; `internal/pluginhost` stays a stub.
  - The six finish-reason mappings are proven against captured upstream fixtures, not only by "tests pass": a SAFETY-blocked
    stream must reach the client as a non-normal terminal state on every path it can take.
  - The stream-truncated classification is proven end to end: an executor-produced 502 with `IsStreamTruncated()` must leave the
    credential out of cooldown and must not trigger a cross-credential replay.

## Authority and Requirements
- authority:
  - `docs/upstream/cliproxyapi-checkpoint.json` — checkpoint `v8.0.44` at `1f917e77c4562623aa49e9804d999524151a2f96` /
    `refs/upstream-checkpoints/cliproxyapi/v8.0.44`; published `2026-10-26T15:49:33Z`; scope strategy `targeted-semantic-ports`,
    include 21 / exclude 3 / defer 15 (written with `upstream_sync.py policy`, `defer` hand-maintained — see Scope Policy).
  - `docs/upstream/cliproxyapi-gap-v8.0.40..v8.0.44.json` — 242 changed paths (match 2, baseline 6, upstream-add-absent 49,
    diverged-absent 90, upstream-delete-present-local 0, semantic-review 95). Derived and gitignored; regenerate with
    `upstream_gap.py`.
  - `docs/upstream/cliproxyapi-ledger-v8.0.40..v8.0.44.md` — 53 non-merge commits across 4 releases; every row carries a
    disposition (`adapt` 27, `defer` 14, `reject` 12).
  - `docs/upstream/cliproxyapi-semantic-review-v8.0.40..v8.0.44.md` — 95 semantic-review paths (adapt 59 / defer 29 / reject 7),
    31 capability slices with local citations, and the deferred-item trigger ledger.
  - `~/quaqua/runs/tieude-up8044.md` — the triage proposal and trigger verdicts this plan consumes.
  - The Lead's scope decision (2026-10-28, relayed in this cycle's brief) — 14 implementation phases P1-P14 in the given order,
    plus `P-gate`; exclude 3 slices, defer 7 slices plus the prior cycle's still-open deferrals and the two owner-gated slices.
  - `CLAUDE.md` / `AGENTS.md` — Postgres-authoritative runtime; no new `web/**/*_test.go`; public SDK changes additive only; local
    xAI executor stays monolithic; llmhub branding retained; pluginhost is a stub.
  - `docs/plans/active/cliproxyapi-v8.0.40-targeted-parity.md` — prior cycle; R1-R24 landed and merged (PR #24, master
    `4a5cce761`), its carried pins are NG14-NG16 here.
  - local git baseline: `4a5cce761` on `quaqua/tieude-up8044` (`quaqua/llmhub-integ` tip after the v8.0.40 cycle was merged and
    its docs artifacts landed at `5da9e206b`).
- requirements:
  - R1 [accepted]: keep `$ref` sibling assertions conjunctive in the Gemini schema cleaner — a conflicting sibling no longer
    overwrites the referenced definition but becomes an `allOf` branch — stop the flattening, required and properties sanitizers
    from weakening a schema that contains a conjunction, and defer array placeholder insertion until after ref/allOf
    resolution. | source: `v8.0.41 29150a2262b4`, `v8.0.43 b2fda98ea831`
  - R2 [accepted]: stop normalizing tool arguments to `{}` unless the declared schema explicitly forbids additional properties,
    and defer an empty `{}` `input_json_delta` until tool_use completion on the Gemini and Antigravity Claude-response paths.
    | source: `v8.0.41 e1fb25219c09`
  - R3 [accepted]: restart the Gemini thought-signature parallel-call group at every non-thought part, so the first
    `functionCall` after a text part receives the bypass sentinel. | source: `v8.0.41 e5434f65c851` (sanitizer half; the
    validator half has no local consumer — NG12)
  - R4 [accepted]: map Codex `phase: commentary` message items to unsigned Claude thinking blocks, and relay them back to Codex as
    own-message commentary through an internal signature marker that is recognised by every signature sanitizer, never forwarded
    to an upstream, and restored to a plain text block on the way out. | source: `v8.0.41 376015c35fea`, `v8.0.44 531721bfcee1`
  - R5 [accepted]: map Gemini/Antigravity `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII`, `IMAGE_SAFETY`,
    `MALFORMED_RESPONSE` and `MALFORMED_FUNCTION_CALL` finish reasons to non-normal terminal states on the OpenAI-chat,
    OpenAI-Responses and Claude paths, emit a cached finish reason even when `usageMetadata` arrives separately or is absent,
    surface a candidate-less `promptFeedback.blockReason` as an explicit 502 with usage preserved, and emit a finish_reason-only
    chunk on OpenAI→Gemini and `STOP` on Codex→Gemini completion. | source: `v8.0.41 3f5d3a161b08`, `v8.0.43 bdde391d53b6`
  - R6 [accepted]: fall back to `ContextLength` / `MaxCompletionTokens` when the Gemini catalog conversion has no dedicated
    `inputTokenLimit` / `outputTokenLimit`. | source: `v8.0.41 83cb1b95eb47`
  - R7 [accepted]: preserve stream-wide tool-call indices and explicit upstream tool-call ids in the Gemini→OpenAI chat
    translator, bind a preceding turn's thought signature to an unsigned function call, treat a completed non-stream candidate
    payload as terminal, and clone the request payload before envelope mutation. | source: `v8.0.42 da21f1e58ed5`
  - R8 [accepted]: share Kimi thinking replay across `k3.1-pro` and `k3.1-pro-highspeed`. | source: `v8.0.42 ab139b45057b`
  - R9 [accepted]: map Claude `output_tokens_details.thinking_tokens` to OpenAI-chat `reasoning_tokens` on streaming,
    non-streaming and usage-only frames. | source: `v8.0.44 43f5386bc3b9`
  - R10 [accepted]: classify a locally synthesized missing-finishReason failure as stream-truncated end to end — executor
    tagging, `usage.Failure.Code`, an auth manager that skips credential cooldown and cross-credential replay, and the
    redisqueue payload — while preserving the public 502 and partial usage. The Home-side scheduler that would consume the
    classification does not exist locally and is out of scope. | source: `v8.0.44 aa4130e64c84`
  - R11 [accepted]: split SSE relay chunks into individual frames on the AI Studio stream path and run model observation, usage
    extraction and translation per frame. | source: `v8.0.43 307803510bce`
  - R12 [accepted]: default an unspecified Vertex credential location to `global` in the executor and in `vertex-import`.
    | source: `v8.0.43 ba06ec58a109`
  - R13 [accepted]: deduplicate tools when promoting historical `additional_tools` in the monolithic xAI executor.
    | source: `v8.0.43 0c61b12cf226`
  - R14 [accepted]: reject parent-directory sequences and both path separators in request-log filenames and validate the final
    component before joining the log directory. | source: `v8.0.42 179d43dc7446`
  - R15 [accepted]: name the accepted API request headers explicitly in CORS preflight responses and echo the requested headers,
    instead of relying on a wildcard that browsers do not apply to `Authorization`. | source: `v8.0.44 058b803b2cce`
  - R16 [accepted]: bound RESP array count, bulk length, frame bytes and line length, and recover per-connection panics in the
    Redis protocol reader. | source: `v8.0.41 888f23824d16`
  - R17 [accepted]: report cancellation when a chunk channel closes together with an already-canceled context — on the delivery
    loop and on the pre-first-chunk bootstrap read — and report a buffered stream error when the upstream websocket data channel
    closes first. | source: `v8.0.43 90fe5b23d350`, `v8.0.44 712e4970bc98`, `v8.0.43 0fe4cc2cfa80`
  - R18 [accepted]: bind a duplex Responses websocket turn to its authenticated principal (401 when it changes mid-stream) and to
    the requested client model taken from request metadata rather than the rewritten payload.
    | source: `v8.0.43 3e7a21e2f3cc`, `v8.0.43 6340fe4312a9`
  - R19 [accepted]: keep the file snapshot binding on persisted auth updates and track persisted revisions so a full scan
    re-reads file-backed auths persisted during that scan. | source: `v8.0.43 32dd2c86b557`
  - R20 [accepted]: restore a re-logged credential to rotation — clear carried `invalid_grant` and non-cooldown unavailable
    state plus stale refresh backoff — while preserving active retry and quota cooldowns. | source: `v8.0.43 21553509662e`
  - R21 [accepted]: downgrade a forced `tool_choice` (`any`/`tool`) to `auto` on Claude 5.5 before thinking preparation using the
    routed upstream model, and fall back to schema prompts for Antigravity Claude instead of forcing `VALIDATED`.
    | source: `v8.0.44 1d45a5f33290`
  - R22 [accepted, invariant]: Postgres remains the authoritative runtime store; management/auth writes persist to it; no feature
    reads authoritative config, credentials or state from local YAML or files. Upstream file-epoch and file-snapshot guards are
    re-expressed as store-version checks. | source: CLAUDE.md
  - R23 [accepted, invariant]: the local xAI executor stays monolithic; port behaviour, not upstream's file split.
    | source: CLAUDE.md
  - R24 [accepted, invariant]: `internal/pluginhost` stays a registration stub; no plugin runtime, `sdk/pluginapi`,
    `sdk/pluginabi`, plugin-model binding or `HasTokenCountTransformer` surface is imported. | source: CLAUDE.md
  - R25 [accepted, invariant]: every `sdk/**` change is additive; existing `SessionAffinitySelector`, auth consumers, API handlers
    and `WebsocketInput` callers keep compiling unchanged. | source: CLAUDE.md
  - R26 [accepted, invariant]: llmhub branding, the embedded management web layout/tokens/i18n, and the release contract stay
    local; no new `web/**/*_test.go`, no README/sponsor/assets/CI/toolchain content from upstream. | source: CLAUDE.md
  - R27 [accepted, invariant]: the upstream `interactions` translator family, the Claude-cloaking identity change and the Codex
    96h refresh lead are not introduced as side effects of any phase; they are deferred by decision (NG7, NG12, NG13).
    | source: this plan's scope decision
  - R-gate [accepted]: the final gate runs `go test ./internal/... ./sdk/... -count=1`, `make embed && make build`,
    `git diff --check`, the scope audit, and an upstream re-resolve that refreshes the checkpoint, gap and ledger.
    | source: upstream checkpoint integrity
  - R-gate-followup [accepted]: if the latest stable upstream release moved after planning, implementation must not silently widen
    scope; the ledger identifies the delta and either includes it through an approved plan refinement or pins it as an explicit
    follow-up. `v8.0.45 ad8bdd3f20be` is already pinned that way (NG17). | source: targeted-scope policy

## Non-goals
- NG1: `pluginhost` — ten commits in range (`v8.0.41 8289ec4c71ad`; `v8.0.42 c9af5290a6d2`, `e42eee6e1ce7`, `35e834da2466`,
  `44d1ddf53887`, `9d08f0d430ed`, `2bb6ac6faa2a`; `v8.0.43 e48d780f9b25`; `v8.0.44 e5321f535caf`, `6fe654bfc6b6`). Invariant
  R24: `internal/pluginhost/lifecycle.go:1-13` is a type-alias registration stub and the local tree has no `sdk/pluginapi` or
  `sdk/pluginabi`. Trigger to pull in: a plugin-runtime initiative is approved as its own cycle.
- NG2: `branding-docs` — README sponsor/extension removal (`v8.0.42 bc0643405a1d`, `v8.0.44 1f917e77c456`). Invariant R26:
  llmhub branding and release docs are not upstream's. Trigger to pull in: never as parity.
- NG3: `test-churn` — race fixes inside upstream's own websocket executor tests (`v8.0.43 84e83441472c`). No user-visible
  behaviour; the local suite keeps its own `internal/runtime/executor/codex_websockets_*_test.go`. Trigger: a local flake traced
  to the same assertion shape.
- NG4: `codex-fullchain-compression` (`v8.0.44 064e008b079f`) — zstd request compression, negotiated downstream compression and a
  shared upstream decoder; needs `internal/api/response_compression.go`, `helps/response_decode.go` and a
  `server.response-compression` config surface (none exist locally). Trigger to pull in: Codex egress bandwidth or cost becomes a
  budget item.
- NG5: `oauth-session-proxy` (`v8.0.43 27f3abfb70a6`) — session-scoped OAuth proxy with per-account persistence. Trigger to pull
  in: `siwc-credential-binding` is approved; the local `oauth_sessions.go:1-498` has no bind session to extend.
- NG6: `discovery-mdns` (`v8.0.44 11e5f67bdb36`, `8a982a521019`, `b214385c1cf3`, `999526c76def`) — split-response resolver,
  bounded caches, draft TXT schema, `github.com/miekg/dns`. Trigger to pull in: an operator depends on LAN discovery across
  fragmented mDNS responses.
- NG7: `interactions-protocol` (`v8.0.44 37bd18b08d9c`, `ede07cafa64a`) — the local tree has no interactions translator family
  (`sdk/translator/formats.go:12` declares the format; `internal/runtime/executor/helps/codex_multi_agent_v2.go:123` records the
  omission). Trigger to pull in: interactions translators are adopted as their own initiative.
- NG8: `devin-lightning-catalog` (`v8.0.43 3dae075de272`) — swe-2 lightning aggregation plus a 462-line catalog rewrite. Trigger
  to pull in: a Devin models/quota cycle, together with `devin-quota`.
- NG9: `home-cluster` (`v8.0.41 e45f942645ca`, `v8.0.42 50147aec62b6`, `v8.0.43 ce433b3727aa`, home half of
  `v8.0.42 0d3ca1d3b7cf`) — latency telemetry, rebalance transport close, subscriber backoff. Trigger to pull in: a multi-node
  `internal/home` cluster is deployed.
- NG10: `store-gitstore-corruption` (store half of `v8.0.42 0d3ca1d3b7cf`) — the local `internal/store/gitstore.go` (1026 lines,
  33 functions) has no `isRepositoryCorruptionError` and no dotgit recovery path to widen. Trigger to pull in: the local git
  store gains a corruption-recovery path.
- NG11: prior-cycle deferrals still open, with their trigger verdicts re-checked this cycle (all unmet — see
  `cliproxyapi-semantic-review-v8.0.40..v8.0.44.md` `## Deferred-item trigger ledger`): `vertex-openapi-routing` (no
  `/endpoints/openapi` route, `helps/vertex_openapi.go` absent), `claude-web-search-cache` (no cache, no demand),
  `codex-remote-compaction-v2` (no remote-compaction v2 surface), `siwc-credential-binding` (no SIWC machinery),
  `guardian-review-credentials` (no code), `tls-handshake-timeout` (no config knob; `helps/utls_client.go` has no handshake
  bound). Each keeps its own trigger; none is pulled in by v8.0.40..v8.0.44.
- NG12: `gemini-thought-signature-validation` — the validator half of `v8.0.41 e5434f65c851`. `internal/signature/
  gemini_validation.go` exports only `ValidateGeminiFunctionCallPairing` (`:31`) and `IsGeminiThoughtSignatureBypass` (`:183`);
  `ValidateGeminiThoughtSignatures` does not exist (grep 0), so the change would add a validator with no caller. Trigger to pull
  in: a caller needs thought-signature validation.
- NG13: `claude-cloak-never` — deferred-awaiting-owner. Not approved by anh iu on 2026-10-26. The v8.0.40..v8.0.44 range does not
  touch the cloak path (`git diff --name-only v8.0.40 v8.0.44 | grep -i cloak` = no match). Never a phase until the owner
  confirms that stripping Claude Code identity on API keys under `cloak_mode=never` is wanted.
- NG14: Codex 96h proactive refresh lead (`codex-auth-refresh`) — deferred-awaiting-owner. `sdk/auth/codex.go:36-37` still returns
  `24 * time.Hour`, and `sdk/auth/codex.go` is not in this range's diff. R20 (`21553509662e`) is the re-login rotation fix from the
  same area and is independent of the cadence: it must not change the lead. Trigger to pull in: the owner signs off on the
  cadence change.
- NG15: carried-open pin R12 — injection of the cloaked `currentDate` into outgoing Claude requests. The retention store exists at
  `internal/runtime/executor/helps/claude_diagnostics.go:151` (`LookupClaudeSessionDatePin`) with no non-test caller (only its own
  helper `ClaudeSessionDatePinKey` at `:138` and internal retention uses at `:155`/`:199`). No phase. Trigger: a shipped consumer
  design for the injection site.
- NG16: carried-open pin P9 F4 — store-level CAS for config. `internal/store/postgresstore.go:360` `SaveConfig(ctx, data)` takes
  no expected version, while `SaveRuntimeSettings(ctx, expectedRevision, …)` at `internal/store/postgres_runtime_controls.go:91`
  does; the two management writers (`internal/api/handlers/management/config_basic.go:149`, `handler.go:562`) are in-process. No
  phase. Trigger: a second writer appears for the config row (multi-instance management writes).
- NG17: carried-open pin P12 — `KeepTopLevelEffort` consumer. Declared at `sdk/cliproxy/executor/types.go:106-108`, no consumer
  (grep 1 = the declaration). No phase. Trigger: an SIWC request-body rule needs the resolved effort (NG5/`siwc-credential-binding`).
- NG18: v8.0.45 absorption — `ad8bdd3f20bef44f0684b8a6096a216b2c76040d`, published `2026-10-27T15:27:00Z`, 18 non-merge commits /
  123 files / +12673-729 against v8.0.44. It is the next cycle's basis, pinned here; this cycle does not port it and does not
  advance the checkpoint past v8.0.44.
- NG19: wholesale merge of the 242 changed paths, or direct copy of upstream files. Refactors carry no user-visible outcome; port
  the behaviour, not the shape.
- NG20: new `web/**/*_test.go` files; any frontend change.
- NG21: splitting the local xAI executor or the local Claude executor into upstream's file layout.
- NG22: `stream-usage-buffer` — upstream's `helps.StreamUsageBuffer` (one buffered-detail type with `Observe`/`Publish`/
  `PublishFailure`, `internal/runtime/executor/helps/usage_helpers.go:1010` upstream, absent locally: grep = one comment in
  `claude_executor_stream_terminal_test.go:28`) and its five callers. P8 resolved its own truncation record with two private
  locals per Gemini-family stream loop instead of importing the type, because the classification requirement does not need a
  usage-architecture cutover (Lead decision, 2026-10-28, `~/quaqua/runs/pipeline-v8044-p8.md`). Trigger to pull in: a second
  executor needs buffered-detail semantics, or a usage-record ordering bug is found outside the Gemini family.

## Scope Policy (this plan)
- source: `docs/upstream/cliproxyapi-checkpoint.json` `scope_policy`, written with
  `python3 .claude/skills/upstream/scripts/upstream_sync.py policy --slug cliproxyapi --strategy targeted-semantic-ports
  --include … --exclude …`, then `defer` hand-maintained.
- include (21): `gemini-schema-conjunctions`, `claude-tool-argument-fidelity`, `gemini-thought-signature-groups`,
  `codex-commentary-relay`, `gemini-finish-reasons`, `gemini-catalog-token-limits`, `claude-thinking-usage`,
  `antigravity-tool-stream-fidelity`, `kimi-thinking-replay-family`, `aistudio-relay-frames`, `vertex-credential-location`,
  `xai-tool-dedup`, `logging-path-traversal`, `api-cors-preflight`, `redis-resp-bounds`, `stream-cancellation-reporting`,
  `responses-ws-principal-binding`, `stream-truncated-cooldown`, `watcher-snapshot-fencing`, `auth-relog-rotation`,
  `claude-55-forced-tools`
- exclude (3): `pluginhost`, `branding-docs`, `test-churn`
- defer (15): `codex-fullchain-compression`, `oauth-session-proxy`, `discovery-mdns`, `interactions-protocol`,
  `devin-lightning-catalog`, `home-cluster`, `store-gitstore-corruption`, `vertex-openapi-routing`, `claude-web-search-cache`,
  `codex-remote-compaction-v2`, `siwc-credential-binding`, `guardian-review-credentials`, `tls-handshake-timeout`,
  `claude-cloak-never`, `codex-auth-refresh`
- caveat: `scope_policy.defer` is maintained by hand because `upstream_sync.py policy` has no `--defer` flag
  (`upstream_sync.py:243-262` writes only `strategy`, `include` and `exclude`; the printed JSON after a `policy` run echoes the
  previous `defer` list verbatim). The trigger text lives in NG4-NG14 below, not in the checkpoint.
- authority order: if this plan and the recorded policy disagree, rerun `policy`; do not edit the plan to match.

## Approach and Risks
- approach: land the two shared-output-producing packages first (`internal/util` schema and tool-argument helpers, then
  `internal/signature`), because the translator phases read them; then the finish-reason/usage mapping that eight translator
  files share; then the per-provider fidelity groups; then the independent single-surface phases (usage, logging, api, handlers,
  watcher); finish with the Claude 5.5 executor change and the gate. Every phase is independently verifiable with its own `go
  test` filter and no phase needs another phase's code to compile.
- single-writer facts that fix the order (the same list is repeated as `depends_on` in each phase):
  - `internal/translator/gemini/claude/gemini_claude_response.go` is written by P2 (`v8.0.41 e1fb25219c09`, the deferred empty
    tool input) and P5 (`v8.0.41 3f5d3a161b08`, the `MALFORMED_RESPONSE` stop reason). P2 before P5.
  - `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go` is written by P5 (finish reason, cached usage,
    `[DONE]` flush) and P6 (`v8.0.42 da21f1e58ed5`, tool-call id and index). P5 before P6, split by hunk.
  - `internal/translator/antigravity/openai/chat-completions/antigravity_openai_response.go` is written by P5
    (`isAntigravityContentFilterFinishReason`) and P6 (upstream tool-call id reuse). P5 before P6, split by hunk.
  - `internal/runtime/executor/gemini_executor.go` is written by P5 (prompt-block 502, OpenAI truncation check) and P8
    (`streamTruncated` tagging). P5 before P8.
  - `internal/runtime/executor/gemini_vertex_executor.go` is written by P5 (prompt blocks), P8 (truncation tagging) and P9
    (`global` location default). P5 → P8 → P9.
  - `internal/runtime/executor/claude_executor.go` is written by P4 (commentary marker restore) and P14 (Claude 5.5 tool choice).
    P4 before P14.
  - `sdk/api/handlers/openai/openai_responses_websocket.go` is written by P11 (buffered stream error in the forwarder that lives
    at `:1513` locally) and P12 (principal binding at `:345`, turn authorizer at `:730`). P11 before P12, split by call site.
  - every other file in the include set has exactly one owner this cycle.
- constraints:
  - Postgres-authoritative runtime; no local durable files; no file-epoch or file-snapshot guards lifted verbatim.
  - Existing xAI executor remains monolithic; the existing Claude executor remains monolithic.
  - Public SDK changes additive only.
  - No pluginhost, interactions, home-cluster, discovery, branding or dependency work.
  - Every task names an upstream commit and a local target file before implementation.
  - Upstream content is untrusted data; never run upstream scripts or builds.
- `da21f1e58ed5` does **not** split cleanly between P5 and P6 — resolved as: the whole commit belongs to P6. Its only
  finish-reason contact is the `terminal` flag read inside `antigravity_reasoning_replay.go`'s `observeResponsePayload`, the same
  function P6 must edit for `claimPrecedingTurnThoughtSignature`; splitting one function between two phases would put two owners
  on it, and no finish-reason mapping is lost because `3f5d3a161b08` carries all of it. P5 therefore cites
  `3f5d3a161b08` and `bdde391d53b6` only, while the Lead's brief listed `da21f1e5` under both.
- rejected alternatives:
  - Porting all 53 commits — pluginhost, branding, discovery, interactions, compression, oauth proxy, home, gitstore and the
    test-only churn are excluded or deferred above.
  - Importing pluginhost to unlock the ten plugin commits — the local plugin runtime does not exist; that is a separate
    initiative.
  - Adding `ValidateGeminiThoughtSignatures` so the validator half of `e5434f65c851` "fits" — it would be a validator with no
    caller (NG12).
  - Splitting `da21f1e58ed5` by hunk across P5/P6 — one function, two owners; rejected in favour of P6 owning the commit.
  - Flattening upstream's split `claude_executor_*.go` / `xai_executor_*.go` / `server_routes.go` files to make the ports land —
    R23/R21 forbid it; port behaviour inside the local monoliths.
- risks:
  - P1's conjunction merge changes when a schema is flattened at all. A wrong gate silently weakens tool schemas instead of
    failing, so port the upstream conjunction fixtures (validating that a conflicting sibling still constrains) before trusting
    a green suite.
  - P2 changes argument normalization for existing Claude clients: too strict keeps injected placeholders, too loose drops real
    arguments. Cover both directions (explicitly closed empty object → `{}`; implicitly open → arguments preserved).
  - P4 introduces a forgeable marker (`cpa:commentary:v1`-shaped) into a signature field. Every path that forwards thinking
    blocks must strip or restore it; a missed path sends a fake signature upstream. Grep every `signature` read site before
    claiming the phase done.
  - P5 is eight translator files plus two executors. Land the terminal-state mapping behind one shared helper per protocol family
    rather than eight ad-hoc switches, or the next cycle re-litigates each file.
  - P6's replay binding is stateful: a wrong `claimPrecedingTurnThoughtSignature` leaks a signature across turns, which is worse
    than the missing signature it replaces. Fixtures must include a turn with two functionCall groups.
  - P8 must not turn the truncation classification into a blanket "never cool"; only a locally synthesized missing-terminal-frame
    failure qualifies. Both directions need a test (truncated 200 → no cooldown, genuine upstream 5xx → cooldown).
  - P8's redisqueue half is additive JSON (`code` with `omitempty`) — no schema version bump and no migration, which also means
    nothing local consumes it yet; the Home-side cooldown skip is not deliverable this cycle (R10's boundary).
  - P11 and P12 touch the same websocket file; P12 rebases on P11. A principal check that is too strict breaks legitimate client
    reconnects, so state the rejection scope (same stream, changed principal) in the test name.
  - P13's conductor change is the only one this cycle; `RefreshFailures`/`NextRefreshAfter` carry-over is easy to over-clear and
    would resurrect a genuinely failing credential.
  - P14's ordering matters (before thinking prep, after payload rules); a wrong order either discards signed history or lets
    payload rules override the downgrade.
- recovery:
  - If a phase cannot map to a local symbol, stop it as `NEEDS_CONTEXT`; do not invent a subsystem.
  - If a parallel merge conflicts, resolve in the phase owner's branch; do not force both phases to continue.
  - If the build breaks after a phase, revert only that phase's files and re-run its focused tests.
  - If a requirement proves impossible without breaking R22-R27, surface it to the owner; do not silently drop it.

## Phases and Verification
- planning_status: planned
- phases:
  - phase_slug: v8044-p1-gemini-schema-conjunctions
    story_id: story-20261028-v8044-p1
    status: planned
    risk: medium
    goal: Keep `$ref` sibling assertions conjunctive, protect schemas that contain a conjunction from the flattening and sanitizing passes, and defer array placeholder insertion until after ref/allOf resolution.
    upstream_sources:
      - v8.0.41 29150a2262b4
      - v8.0.43 b2fda98ea831
    depends_on: none
    requirements: R1
    allowed_surfaces:
      - `internal/util/gemini_schema.go` (+ `internal/util/gemini_schema_test.go`)
    avoided_surfaces:
      - `internal/util/translator.go` (P2 owns it)
      - porting upstream's 496-line file shape or the `test/gemini_ref_sibling_test.go` fixture path
    tasks:
      - task: [R1] Add the conjunction-preserving ref merge (target + siblings become `allOf` branches when assertions collide) to `inlineLocalRefsRooted`/`resolveLocalRefs` at `internal/util/gemini_schema.go:1806-1818`, per `29150a2262b4`.
      - task: [R1] Gate the flattening and sanitizing passes on the presence of a conjunction: `mergeAllOf` (`:128`), `flattenAnyOfOneOf`, `flattenTypeArrays`, `cleanupRequiredFields`, `sanitizeObjectProperties`, `sanitizeArrayItems`, per `29150a2262b4`.
      - task: [R1] Move the array placeholder out of the unconditional `normalizeMalformedSchemaObjects(jsonStr, true)` call at `:117` so it runs only after refs and conjunctions are resolved, per `b2fda98ea831`.
    checks:
      - check: `go test ./internal/util/ -run 'Gemini|Schema|Ref' -count=1`

  - phase_slug: v8044-p2-claude-tool-argument-fidelity
    story_id: story-20261028-v8044-p2
    status: planned
    risk: medium
    goal: Only normalize tool arguments to `{}` for schemas that explicitly forbid additional properties, and defer an empty `{}` tool input until tool completion on the Gemini and Antigravity Claude-response paths.
    upstream_sources:
      - v8.0.41 e1fb25219c09
    depends_on: none
    requirements: R2
    allowed_surfaces:
      - `internal/util/translator.go`, `internal/util/sanitize_test.go`
      - `internal/translator/antigravity/claude/antigravity_claude_response.go` (+ test)
      - `internal/translator/gemini/claude/gemini_claude_response.go` (the deferred-empty-input hunk only; P5 owns the stop-reason set there)
    avoided_surfaces:
      - changing the placeholder injection itself (`claudeDeclaredPropertyPlaceholders`, `internal/util/translator.go:819`)
      - touching the `gemini_claude_response.go` stop-reason switch (P5)
    tasks:
      - task: [R2] Require `info.forbidsDynamic` in `ClaudeToolIsParameterless` (`internal/util/translator.go:739-753`) and in the whole-object gates of `cleanClaudeToolObject` (`:873-890`), `cleanClaudeToolArgumentsByTranslatedSchema` and `CleanClaudeToolArguments`, per `e1fb25219c09`.
      - task: [R2] Add deferred empty tool input (`CurrentToolPendingEmpty` + `flushPendingToolInput`) to `antigravity_claude_response.go` around the `input_json_delta` sites at `:281-291` and `:332-342`, and the equivalent deferral in `gemini_claude_response.go`, per `e1fb25219c09`.
    checks:
      - check: `go test ./internal/util/ -run 'Tool|Parameterless|Argument' -count=1`
      - check: `go test ./internal/translator/antigravity/claude/ ./internal/translator/gemini/claude/ -run 'Tool|Argument|Input' -count=1`

  - phase_slug: v8044-p3-gemini-thought-signature-groups
    story_id: story-20261028-v8044-p3
    status: planned
    risk: low
    goal: Restart the Gemini parallel-call group at every non-thought part so the first functionCall after a text part is signed.
    upstream_sources:
      - v8.0.41 e5434f65c851
    depends_on: none
    requirements: R3
    allowed_surfaces:
      - `internal/signature/gemini_sanitize.go` (+ `internal/signature/gemini_sanitize_test.go`)
    avoided_surfaces:
      - `internal/signature/gemini_validation.go` (NG12: no local validator to change)
      - changing the bypass-sentinel policy itself (`GeminiReplaySignatureOrBypass`)
    tasks:
      - task: [R3] Apply the non-thought-part reset at both guard sites — `SanitizeGeminiRequestThoughtSignatures` (`internal/signature/gemini_sanitize.go:82-84`) and `geminiContentsThoughtSignaturesNeedSanitize` (`:171-173`) — per `e5434f65c851`.
      - task: [R3] Cover a model turn of `[text, functionCall, functionCall]` and of `[thought, functionCall]`: the first call after text is signed, the one after a thought part is not.
    checks:
      - check: `go test ./internal/signature/ -run 'Gemini|Signature|Group' -count=1`

  - phase_slug: v8044-p4-codex-commentary-relay
    story_id: story-20261028-v8044-p4
    status: planned
    risk: medium
    goal: Map Codex `phase: commentary` items to unsigned Claude thinking blocks and relay them back through an internal marker that is never forwarded upstream and is restored to a text block by every signature sanitizer.
    upstream_sources:
      - v8.0.41 376015c35fea
      - v8.0.44 531721bfcee1
    depends_on: none
    requirements: R4
    allowed_surfaces:
      - `internal/signature/claude_messages_sanitize.go` (marker constant, `IsCodexCommentarySignatureMarker`, `RestoreCodexCommentaryMarkers`, and the marker branch in `SanitizeClaudeMessagesSignaturesForTarget`)
      - `internal/translator/codex/claude/codex_claude_response.go` (commentary block state, `HasTextDelta` reset per message item)
      - `internal/translator/codex/claude/codex_claude_request.go` (marker → own commentary message item)
      - `internal/runtime/executor/claude_executor.go` (call the restore helper on the Claude request path)
      - `internal/translator/antigravity/claude/antigravity_claude_request.go`, `internal/translator/gemini/claude/gemini_claude_request.go`, `internal/translator/openai/claude/openai_claude_request.go`
      - the three matching `*_test.go` files, `internal/runtime/executor/claude_executor_test.go`
    avoided_surfaces:
      - `internal/translator/interactions/claude/interactions_claude_request.go` (no local counterpart — NG7)
      - changing the provider signature compatibility table (`provider_compatibility.go`)
      - the Claude 5.5 tool-choice path (P14)
    tasks:
      - task: [R4] Add the marker and the two helpers to `internal/signature/claude_messages_sanitize.go` and route the marker through `SanitizeClaudeMessagesSignaturesForTarget` so it becomes a text block and is counted as a dropped signature, per `531721bfcee1`.
      - task: [R4] Map `phase: commentary` output items to an unsigned thinking block in `codex_claude_response.go` (stream and non-stream), reset `HasTextDelta` across message item boundaries and keep pending reasoning signatures isolated, per `376015c35fea`.
      - task: [R4] Restore the marker to an own `phase: commentary` assistant message item in `codex_claude_request.go` and strip it in the four other Claude-request translators, per `531721bfcee1`.
      - task: [R4] Wire `RestoreCodexCommentaryMarkers` into the monolithic `claude_executor.go` request path (upstream calls it from the split execute/stream files).
    checks:
      - check: `go test ./internal/signature/ -run 'Commentary|Marker|Sanitize' -count=1`
      - check: `go test ./internal/translator/codex/claude/ -run 'Commentary|Phase|Replay' -count=1`
      - check: `go test ./internal/runtime/executor/ -run 'Commentary|Claude' -count=1`

  - phase_slug: v8044-p5-gemini-finish-reasons
    story_id: story-20261028-v8044-p5
    status: planned
    risk: medium
    goal: Map Gemini/Antigravity safety and malformed finish reasons to non-normal terminal states on every protocol path, keep a cached finish reason when usage arrives late, surface candidate-less prompt blocks as 502, and fall back to `ContextLength`/`MaxCompletionTokens` in the Gemini catalog.
    upstream_sources:
      - v8.0.41 3f5d3a161b08
      - v8.0.43 bdde391d53b6
      - v8.0.41 83cb1b95eb47
    depends_on: v8044-p2-claude-tool-argument-fidelity
    requirements: R5, R6
    allowed_surfaces:
      - `internal/runtime/executor/gemini_executor.go`, `internal/runtime/executor/gemini_vertex_executor.go` (prompt-block surfacing + the OpenAI truncation check only; P8 owns their truncation tagging, P9 owns the vertex location)
      - `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go` (finish-reason mapping, cached usage, `[DONE]` flush)
      - `internal/translator/gemini/openai/responses/gemini_openai-responses_response.go`
      - `internal/translator/gemini/claude/gemini_claude_response.go` (stop-reason set only)
      - `internal/translator/antigravity/openai/chat-completions/antigravity_openai_response.go` (content-filter mapping only)
      - `internal/translator/codex/gemini/codex_gemini_response.go`, `internal/translator/openai/gemini/openai_gemini_response.go`
      - `internal/registry/model_registry.go` (+ `model_registry_cache_test.go`, `internal/api/server_test.go`)
      - the matching translator and executor `*_test.go` files
    avoided_surfaces:
      - `internal/api/server_routes.go` (does not exist; local handlers are in `internal/api/server.go` — do not restructure)
      - `antigravity_reasoning_replay.go`, the tool-call id/index hunks (P6)
      - the vertex location default (P9) and truncation tagging (P8)
    tasks:
      - task: [R5] Add the safety/malformed finish-reason set as one shared classifier per family and use it in `resolveGeminiClaudeStopReason` (`gemini_claude_response.go:53-58`, adding `MALFORMED_RESPONSE`), `resolveOpenAIFinishReason` (antigravity, `content_filter`), the Gemini→OpenAI-chat mapping (`gemini_openai_response.go:245-250`) and the Responses terminal state (new `geminiResponsesTerminalState` equivalent), per `3f5d3a161b08`.
      - task: [R5] Cache the finish reason and pending usage in `gemini_openai_response.go` so `[DONE]` (`:77`) and a usage-only chunk still emit the terminal frame, and emit `STOP` on Codex→Gemini `response.completed` (`codex_gemini_response.go:336-346`) and keep a finish_reason-only chunk on OpenAI→Gemini (`openai_gemini_response.go:150-152`), per `3f5d3a161b08` and `bdde391d53b6`.
      - task: [R5] Detect a candidate-less `promptFeedback.blockReason` in both Gemini executors and return a 502 that still records usage, per `3f5d3a161b08`.
      - task: [R6] Fall back to `ContextLength`/`MaxCompletionTokens` in `convertModelToMap` (`internal/registry/model_registry.go:1877-1882`), per `83cb1b95eb47`.
    checks:
      - check: `go test ./internal/translator/gemini/... ./internal/translator/antigravity/... ./internal/translator/codex/gemini/ ./internal/translator/openai/gemini/ -run 'Finish|Reason|Terminal|Usage' -count=1`
      - check: `go test ./internal/runtime/executor/ -run 'Gemini|PromptBlock|Finish' -count=1`
      - check: `go test ./internal/registry/ -run 'TokenLimit|Catalog' -count=1`

  - phase_slug: v8044-p6-antigravity-tool-stream-fidelity
    story_id: story-20261028-v8044-p6
    status: planned
    risk: medium
    goal: Preserve stream-wide tool-call indices and explicit upstream tool-call ids, bind a preceding turn's thought signature to an unsigned function call, mark completed non-stream payloads terminal, clone the request payload before mutation, and share Kimi replay across `k3.1-pro` variants.
    upstream_sources:
      - v8.0.42 da21f1e58ed5
      - v8.0.42 ab139b45057b
    depends_on: v8044-p5-gemini-finish-reasons
    requirements: R7, R8
    allowed_surfaces:
      - `internal/runtime/executor/antigravity_reasoning_replay.go` (owning phase for the whole file this cycle)
      - `internal/runtime/executor/antigravity_executor_request.go` (payload clone in `geminiToAntigravity`)
      - `internal/runtime/executor/kimi_thinking_replay.go` (+ test)
      - `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go` (tool-call id/index hunks only)
      - `internal/translator/antigravity/openai/chat-completions/antigravity_openai_response.go` (tool-call id hunk only)
      - `internal/runtime/executor/xai_executor_test.go`-style fixtures are *not* in scope; use new cases in the antigravity/kimi tests
    avoided_surfaces:
      - the finish-reason mapping P5 landed in the same two translator files
      - `helps/antigravity_structured_output.go` (P14)
    tasks:
      - task: [R7] Use stream-wide indices and keep explicit upstream ids in `gemini_openai_response.go` (streaming `:241-256`, non-stream `:437-450`) and `antigravity_openai_response.go:190`, per `da21f1e58ed5`.
      - task: [R7] Add `claimPrecedingTurnThoughtSignature`, the bare `candidates.0.*` shape and the terminal marking in `antigravity_reasoning_replay.go`, plus the context API-key caller-scope fallback, per `da21f1e58ed5`.
      - task: [R7] Clone the payload in `geminiToAntigravity` before envelope mutation (`antigravity_executor_request.go:646-649`), per `da21f1e58ed5`.
      - task: [R8] Map `k3.1-pro`/`k3.1-pro-highspeed` to one family in `kimiThinkingReplayModelFamily` (`internal/runtime/executor/kimi_thinking_replay.go:33-42`), per `ab139b45057b`.
    checks:
      - check: `go test ./internal/translator/gemini/openai/chat-completions/ ./internal/translator/antigravity/... -run 'ToolCall|Index|ID' -count=1`
      - check: `go test ./internal/runtime/executor/ -run 'Antigravity|Replay|Kimi|Signature' -count=1`

  - phase_slug: v8044-p7-claude-thinking-usage
    story_id: story-20261028-v8044-p7
    status: planned
    risk: low
    goal: Map Claude `output_tokens_details.thinking_tokens` to OpenAI-chat `reasoning_tokens` on every usage emit site.
    upstream_sources:
      - v8.0.44 43f5386bc3b9
    depends_on: none
    requirements: R9
    allowed_surfaces:
      - `internal/translator/claude/openai/chat-completions/claude_openai_response.go` (+ test)
    avoided_surfaces:
      - the Responses-path usage helpers (`helps/usage_helpers.go` — P8)
    tasks:
      - task: [R9] Extend `claudeUsageTokens` with `ThinkingTokens`/`HasThinkingTokens` and apply them at the three usage emit sites (stream, usage-only, non-stream), per `43f5386bc3b9`.
    checks:
      - check: `go test ./internal/translator/claude/openai/chat-completions/ -run 'Usage|Thinking' -count=1`

  - phase_slug: v8044-p8-stream-truncated-cooldown
    story_id: story-20261028-v8044-p8
    status: planned
    risk: medium
    goal: A locally synthesized missing-terminal-frame failure must not cool the credential or replay the turn on another credential, while the public 502 and partial usage stay as they are.
    upstream_sources:
      - v8.0.44 aa4130e64c84
    depends_on: v8044-p5-gemini-finish-reasons
    requirements: R10
    allowed_surfaces:
      - `internal/runtime/executor/gemini_executor.go` (truncation tag at `:389`), `internal/runtime/executor/gemini_vertex_executor.go` (its two sites)
      - `internal/runtime/executor/openai_compat_executor.go` (`statusErr.streamTruncated`, `IsStreamTruncated()`)
      - `internal/runtime/executor/helps/usage_helpers.go` (`failFromErrors` → `Failure.Code`)
      - `sdk/cliproxy/executor/types.go` (`StreamTruncatedError`), `sdk/cliproxy/usage/manager.go` (`Failure.Code`, `FailureStreamTruncatedCode`), `sdk/cliproxy/auth/errors.go` (`ErrorCodeStreamTruncated`), `sdk/cliproxy/auth/conductor_cooldown.go` (skip cooldown), `sdk/cliproxy/auth/conductor.go` if the skip needs the lifecycle path
      - `internal/redisqueue/plugin.go` (`failDetail.Code` with `omitempty`)
    avoided_surfaces:
      - `sdk/cliproxy/executor/types.go`'s `SelectionID`/plugin-usage surface (deferred with NG1)
      - changing the public status code or dropping partial usage
      - a migration or schema-version bump (not needed — see the plan's risk note)
    tasks:
      - task: [R10] Add `streamTruncated` to `statusErr`, tag both executors' synthesized failures, and expose `IsStreamTruncated()` per `aa4130e64c84`.
      - task: [R10] Add the additive SDK surface (`StreamTruncatedError`, `usage.Failure.Code`, `FailureStreamTruncatedCode`, `ErrorCodeStreamTruncated`) and thread the code through `failFromErrors`.
      - task: [R10] Skip credential cooldown and cross-credential replay for a stream-truncated failure in the auth manager, keeping every other failure path untouched.
      - task: [R10] Emit `code` in the redisqueue `failDetail` (`omitempty`), accepting that no local consumer reads it yet.
    checks:
      - check: `go test ./internal/runtime/executor/ -run 'Truncat|Usage|Failure' -count=1`
      - check: `go test ./sdk/cliproxy/auth/ ./sdk/cliproxy/usage/ -run 'Truncat|Cooldown|Failure' -count=1`
      - check: `go test ./internal/redisqueue/ -run 'Fail|Usage' -count=1`

  - phase_slug: v8044-p9-executor-small-surface
    story_id: story-20261028-v8044-p9
    status: planned
    risk: low
    goal: Translate every AI Studio relay frame, default the Vertex location to `global`, deduplicate promoted xAI tools, and reject unsafe request-log filenames.
    upstream_sources:
      - v8.0.43 307803510bce
      - v8.0.43 ba06ec58a109
      - v8.0.43 0c61b12cf226
      - v8.0.42 179d43dc7446
    depends_on: v8044-p8-stream-truncated-cooldown
    requirements: R11, R12, R13, R14
    allowed_surfaces:
      - `internal/runtime/executor/aistudio_executor.go` (+ new multi-frame test)
      - `internal/runtime/executor/gemini_vertex_executor.go` (location default only), `internal/cmd/vertex_import.go`
      - `internal/runtime/executor/xai_executor.go` (promotion sites `:1812`, `:2044-2062`, `:2145`) + `internal/runtime/executor/xai_executor_test.go`
      - `internal/logging/request_logger.go` (`sanitizeForFilename` `:839-856`, the unique-file creator)
    avoided_surfaces:
      - `helps/vertex_openapi.go` (does not exist — NG11)
      - splitting the xAI executor into upstream's file layout
      - the Redis-dialer half of `888f23824d16` in `internal/home/certificate.go` (NG9)
    tasks:
      - task: [R11] Add `splitRelayFrames`/`startsWithSSEField` and run the per-frame path inside the AI Studio stream loop (`aistudio_executor.go:325-330`), per `307803510bce`.
      - task: [R12] Default an unspecified Vertex location to `global` in `vertexCreds` (`:1124`), `vertexBaseURL` (`:1164-1165`) and `vertex-import`, per `ba06ec58a109`.
      - task: [R13] Deduplicate promoted historical `additional_tools` in the monolithic `xai_executor.go`, per `0c61b12cf226`.
      - task: [R14] Strip both separators and parent sequences in `sanitizeForFilename`, and validate the final component before joining the log directory, per `179d43dc7446`.
    checks:
      - check: `go test ./internal/runtime/executor/ -run 'AIStudio|Multiframe|Vertex|XAI|Tools' -count=1`
      - check: `go test ./internal/logging/ -run 'RequestLogger|Filename|Traversal' -count=1`

  - phase_slug: v8044-p10-api-small-surface
    story_id: story-20261028-v8044-p10
    status: planned
    risk: low
    goal: Make CORS preflight work for `Authorization` and bound the Redis protocol reader.
    upstream_sources:
      - v8.0.44 058b803b2cce
      - v8.0.41 888f23824d16
    depends_on: none
    requirements: R15, R16
    allowed_surfaces:
      - `internal/api/server.go` (`corsMiddleware` `:1645-1649` and a header-value helper)
      - `internal/api/redis_queue_protocol.go` (`readRESPArray` `:381-388`, the connection handler) + its integration test
      - `internal/api/protocol_multiplexer.go` (a `baseline` blob — applies as upstream)
    avoided_surfaces:
      - `internal/home/certificate.go` (Redis dialer — NG9)
      - response compression middleware (NG4)
    tasks:
      - task: [R15] Replace `Access-Control-Allow-Headers: *` with an explicit list plus echo of `Access-Control-Request-Headers` on preflight, per `058b803b2cce`.
      - task: [R16] Add array/bulk/line/frame limits and per-connection panic recovery in `redis_queue_protocol.go` and the multiplexer, per `888f23824d16`.
    checks:
      - check: `go test ./internal/api/ -run 'CORS|Preflight|RESP|Protocol' -count=1`

  - phase_slug: v8044-p11-stream-cancellation-reporting
    story_id: story-20261028-v8044-p11
    status: planned
    risk: medium
    goal: A chunk channel that closes together with a canceled context is reported as canceled, including before the first chunk, and a buffered stream error is reported when the upstream websocket data channel closes first.
    upstream_sources:
      - v8.0.43 90fe5b23d350
      - v8.0.44 712e4970bc98
      - v8.0.43 0fe4cc2cfa80
    depends_on: none
    requirements: R17
    allowed_surfaces:
      - `sdk/api/handlers/handlers.go` (bootstrap read `:997-1010`, the delivery loop, a new `nextStreamChunk` helper)
      - `sdk/api/handlers/openai/openai_responses_websocket.go` (the forwarder at `:1513` only)
      - their `*_test.go` files
    avoided_surfaces:
      - the authentication and turn-authorizer functions in the websocket file (P12)
      - restructuring `sdk/api/handlers` into upstream's per-concern files
    tasks:
      - task: [R17] Add `nextStreamChunk(ctx, pending, …, chunks)` returning `(chunk, ok, canceled)` with the closed-channel-plus-canceled-context rule, and route both the delivery loop and the bootstrap read through it, per `90fe5b23d350` and `712e4970bc98`.
      - task: [R17] Report a buffered stream error in the forwarder when the upstream data channel closes first, per `0fe4cc2cfa80`.
    checks:
      - check: `go test ./sdk/api/handlers/... -run 'Stream|Cancel|Delivery|Bootstrap' -count=1`

  - phase_slug: v8044-p12-responses-ws-principal-binding
    story_id: story-20261028-v8044-p12
    status: planned
    risk: high
    goal: A duplex Responses websocket turn is bound to the principal that authorized the stream and to the requested client model, and a mid-stream principal change is rejected.
    upstream_sources:
      - v8.0.43 3e7a21e2f3cc
      - v8.0.43 6340fe4312a9
    depends_on: v8044-p11-stream-cancellation-reporting
    requirements: R18
    allowed_surfaces:
      - `sdk/api/handlers/openai/openai_responses_websocket.go` (`authenticateResponsesWebsocketTurn` `:345`, the turn authorizer `:730`) + tests
      - `internal/runtime/executor/codex_websockets_duplex.go` (`boundModel` `:184-199`) + test
    avoided_surfaces:
      - the forwarder function (P11)
      - `helps/payload_helpers.go` (`PayloadRequestedModel` already exists at `:857`; consume it, do not re-implement)
    tasks:
      - task: [R18] Return the authenticated principal from `authenticateResponsesWebsocketTurn`, bind it before the turn loop and reject a changed principal with 401, per `3e7a21e2f3cc`.
      - task: [R18] Hoist `boundModel` out of `authorizePayload` and resolve it via `helps.PayloadRequestedModel(opts, initialModel)`, then use it for the continuation comparison, per `6340fe4312a9`.
    checks:
      - check: `go test ./sdk/api/handlers/openai/ -run 'Websocket|Principal|Steering' -count=1`
      - check: `go test ./internal/runtime/executor/ -run 'Duplex|BoundModel' -count=1`

  - phase_slug: v8044-p13-watcher-and-auth-rotation
    story_id: story-20261028-v8044-p13
    status: planned
    risk: medium
    goal: Keep the file snapshot binding and persisted revisions on the watcher, and restore a re-logged credential to rotation without clearing real cooldowns.
    upstream_sources:
      - v8.0.43 32dd2c86b557
      - v8.0.43 21553509662e
    depends_on: none
    requirements: R19, R20
    allowed_surfaces:
      - `internal/watcher/{clients.go,dispatcher.go,watcher.go}` (+ tests)
      - `sdk/cliproxy/auth/conductor.go` (`updateInternalLocked` `:2374-2375`, `clearUnauthorizedModelStates`) + `conductor_refresh.go` if the split exists locally
    avoided_surfaces:
      - `sdk/cliproxy/auth/codex_siwc_bind.go` (does not exist — NG11)
      - `sdk/auth/codex.go` (the refresh lead is NG14)
    tasks:
      - task: [R19] Stop clearing the file snapshot in `dispatchPersistedAuthUpdateWithRevision` (`internal/watcher/dispatcher.go:472`), add `persistedAuthRevisions`, and let a full scan re-read auths persisted during the scan, per `32dd2c86b557`.
      - task: [R20] Clear carried `invalid_grant`/unavailable state and stale refresh backoff only when credentials changed, preserving active retry and quota cooldowns, per `21553509662e`.
    checks:
      - check: `go test ./internal/watcher/ -run 'Snapshot|Revision|Persist|Scan' -count=1`
      - check: `go test ./sdk/cliproxy/auth/ -run 'Refresh|Rotation|Unauthorized|Cooldown' -count=1`

  - phase_slug: v8044-p14-claude-55-forced-tools
    story_id: story-20261028-v8044-p14
    status: planned
    risk: medium
    goal: Downgrade a forced tool choice on Claude 5.5 before thinking preparation and fall back to schema prompts for Antigravity Claude instead of forcing `VALIDATED`.
    upstream_sources:
      - v8.0.44 1d45a5f33290
    depends_on: v8044-p4-codex-commentary-relay
    requirements: R21
    allowed_surfaces:
      - `internal/runtime/executor/helps/claude_tool_choice.go` (new), `internal/runtime/executor/helps/antigravity_structured_output.go` (new)
      - `internal/runtime/executor/claude_executor.go` (Execute, ExecuteStream, CountTokens paths)
      - `internal/runtime/executor/antigravity_executor.go` (request build and token count)
      - their tests
    avoided_surfaces:
      - the commentary-marker restore path (P4)
      - `internal/thinking/**` unless a shared model-name parser is genuinely needed (prefer `thinking.ParseSuffix`, already imported locally)
    tasks:
      - task: [R21] Add `NormalizeClaudeToolChoice(provider, model, payload)` gated on the routed upstream model and call it before `ApplyRequestThinking` in Execute, ExecuteStream and CountTokens, per `1d45a5f33290`.
      - task: [R21] Add the Antigravity Claude-5.5 structured-output fallback and skip `VALIDATED` when a schema prompt was prepared, in both the request build and the token-count path, per `1d45a5f33290`.
    checks:
      - check: `go test ./internal/runtime/executor/ -run 'ClaudeTool|ToolChoice|Antigravity|Structured' -count=1`

  - phase_slug: v8044-p-gate
    story_id: story-20261028-v8044-gate
    status: planned
    risk: medium
    goal: Prove the cycle complete: full card test, embed + build, clean diff, scope audit against the invariants, refreshed upstream basis, and an explicit pin for anything newer than v8.0.44.
    upstream_sources:
      - checkpoint v8.0.44 1f917e77c456
    depends_on: v8044-p1-gemini-schema-conjunctions, v8044-p2-claude-tool-argument-fidelity, v8044-p3-gemini-thought-signature-groups, v8044-p4-codex-commentary-relay, v8044-p5-gemini-finish-reasons, v8044-p6-antigravity-tool-stream-fidelity, v8044-p7-claude-thinking-usage, v8044-p8-stream-truncated-cooldown, v8044-p9-executor-small-surface, v8044-p10-api-small-surface, v8044-p11-stream-cancellation-reporting, v8044-p12-responses-ws-principal-binding, v8044-p13-watcher-and-auth-rotation, v8044-p14-claude-55-forced-tools
    requirements: R-gate, R-gate-followup
    allowed_surfaces:
      - `docs/upstream/**` checkpoint, gap and ledger refresh
      - `docs/plans/active/cliproxyapi-v8.0.44-targeted-parity.md` (gate result and closure sections only)
    avoided_surfaces:
      - silent scope growth
      - excluded and deferred slices
    tasks:
      - task: [R-gate] Run `go test ./internal/... ./sdk/... -count=1`, `make embed && make build`, and `git diff --check` on the integration tip.
      - task: [R-gate] Run `python3 .claude/skills/upstream/scripts/upstream_sync.py sync --slug cliproxyapi`, then `upstream_gap.py` and `upstream_ledger.py`; record the refreshed checkpoint and the delta.
      - task: [R-gate-followup] If a newer stable release exists, pin it as an explicit follow-up with its own ledger rows; do not port it inside this cycle without an approved plan refinement. `v8.0.45 ad8bdd3f20be` is already recorded in NG18.
      - task: [R22-R27] Audit the cycle diff: Postgres authority, monolithic xAI and Claude executors, no pluginhost/pluginapi/pluginabi paths, additive-only `sdk/**`, no `web/**/*_test.go`, no branding paths, no interactions translator files.
    checks:
      - check: `go test ./internal/... ./sdk/... -count=1`
      - check: `make embed && make build`
      - check: `git diff --check`
      - check: `python3 .claude/skills/upstream/scripts/upstream_sync.py sync --slug cliproxyapi`

### Phase map (one row per phase, with the shared-file note)

Slice numbers refer to `docs/upstream/cliproxyapi-semantic-review-v8.0.40..v8.0.44.md` `## Capability slices`.

| Phase | Slices | Requirements | Depends on | Shared-file note |
| --- | --- | --- | --- | --- |
| `v8044-p1-gemini-schema-conjunctions` | #1 | R1 | none | sole owner of `internal/util/gemini_schema.go` |
| `v8044-p2-claude-tool-argument-fidelity` | #2 | R2 | none | first of two owners of `gemini_claude_response.go` |
| `v8044-p3-gemini-thought-signature-groups` | #3 | R3 | none | sole owner of `internal/signature/gemini_sanitize.go` |
| `v8044-p4-codex-commentary-relay` | #4 | R4 | none | first of two owners of `claude_executor.go` |
| `v8044-p5-gemini-finish-reasons` | #5, #6 | R5, R6 | P2 | second owner of `gemini_claude_response.go`; first of `gemini_openai_response.go`, `antigravity_openai_response.go`, `gemini_executor.go`, `gemini_vertex_executor.go` |
| `v8044-p6-antigravity-tool-stream-fidelity` | #8, #9 | R7, R8 | P5 | second owner of the two chat translators; sole owner of `antigravity_reasoning_replay.go` and `kimi_thinking_replay.go` |
| `v8044-p7-claude-thinking-usage` | #7 | R9 | none | sole owner of `claude_openai_response.go` |
| `v8044-p8-stream-truncated-cooldown` | #18 | R10 | P5 | second owner of both Gemini executors; sole owner of the usage/auth SDK surface |
| `v8044-p9-executor-small-surface` | #10, #11, #12, #13 | R11-R14 | P8 | third owner of `gemini_vertex_executor.go`; sole owner of `aistudio_executor.go`, the monolith `xai_executor.go`, `request_logger.go` |
| `v8044-p10-api-small-surface` | #14, #15 | R15, R16 | none | sole owner of `internal/api/server.go` and the RESP reader |
| `v8044-p11-stream-cancellation-reporting` | #16 | R17 | none | first of two owners of `openai_responses_websocket.go` (forwarder) |
| `v8044-p12-responses-ws-principal-binding` | #17 | R18 | P11 | second owner of `openai_responses_websocket.go` (auth/turns); sole owner of `codex_websockets_duplex.go` |
| `v8044-p13-watcher-and-auth-rotation` | #19, #20 | R19, R20 | none | sole conductor owner this cycle |
| `v8044-p14-claude-55-forced-tools` | #21 | R21 | P4 | second owner of `claude_executor.go` |
| `v8044-p-gate` | — | R-gate, R-gate-followup | P1-P14 | docs/upstream only |

## Parallel Execution Directive
- Phase count: the scope decision in the brief says "13 phases"; the enumerated list is P1-P14, i.e. **14 implementation phases plus
  `P-gate`**. This plan follows the enumeration.
- Wave 1 (no dependency, disjoint files): `P1`, `P2`, `P3`, `P4`, `P7`, `P10`, `P11`, `P13`.
- Wave 2: `P5` (needs `P2` — `gemini_claude_response.go`), `P14` (needs `P4` — `claude_executor.go`), `P12` (needs `P11` —
  `openai_responses_websocket.go`).
- Wave 3: `P6` (needs `P5` — two translator files), `P8` (needs `P5` — both Gemini executors).
- Wave 4: `P9` (needs `P8` — `gemini_vertex_executor.go`).
- Wave 5: `P-gate` after every implementation phase merges.
- Files written by more than one phase this cycle, and how the order resolves them:

  | File | Owners in order | Resolution |
  | --- | --- | --- |
  | `internal/translator/gemini/claude/gemini_claude_response.go` | P2 → P5 | P2 owns the deferred empty input; P5 owns the stop-reason set. P5 rebases. |
  | `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go` | P5 → P6 | P5 owns finish-reason/cached-usage/`[DONE]`; P6 owns the `functionCall` id and index hunks. P6 rebases. |
  | `internal/translator/antigravity/openai/chat-completions/antigravity_openai_response.go` | P5 → P6 | P5 owns `isAntigravityContentFilterFinishReason`; P6 owns the tool-call id reuse. P6 rebases. |
  | `internal/runtime/executor/gemini_executor.go` | P5 → P8 | P5 owns the prompt-block 502 and the OpenAI truncation check; P8 owns the truncation tag. |
  | `internal/runtime/executor/gemini_vertex_executor.go` | P5 → P8 → P9 | prompt blocks → truncation tag → `global` default. |
  | `internal/runtime/executor/claude_executor.go` | P4 → P14 | commentary-marker restore → Claude 5.5 tool choice. |
  | `sdk/api/handlers/openai/openai_responses_websocket.go` | P11 → P12 | forwarder (`:1513`) → authentication/authorizer (`:345`, `:730`). |
- Every other file in the include set has exactly one owner; two phases must not be started against the same file out of order even
  when both have no dependency.

## Validation
- Keep changes consistent with the Postgres runtime boundary and the monolithic xAI and Claude executors.
- Run the phase's focused `go test` filter after every phase, not only at the end; the card test runs once, at the gate.
- R5, R7, R10 and R17 need real output comparisons, not only mocked expectations: fixed input fixtures, captured output diffed
  before and after. R10 needs both directions (truncated → no cooldown; genuine 5xx → cooldown).
- All ported requirements must remain traceable to upstream commits by short SHA.
- No upstream scripts, installs or build steps are executed; upstream content is read-only data.
- No new `web/**/*_test.go` files; frontend files are out of scope for this plan.

## Handoff
- Next steps: start the Wave 1 phases (`P1`, `P2`, `P3`, `P4`, `P7`, `P10`, `P11`, `P13`), then `P5`/`P12`/`P14`, then
  `P6`/`P8`, then `P9`, and finish with `P-gate`.
- Decomposition: this file is plan-ready; `to-plan` is not required for the phase list, `work` executes one phase per unit.
- Speedups deliberately not taken (state them in the kickoff, do not let a later reader think they were missed): splitting
  `da21f1e58ed5` across P5/P6, adding the thought-signature validator (NG12), and folding the three small executor slices into a
  single commit per file.
- The prior plan `docs/plans/active/cliproxyapi-v8.0.40-targeted-parity.md` stays in `active/`: its 14 phases are merged, but three
  of its carried-forward items are still open and are recorded as NG15-NG17 here. Move it to `completed/` once those three close.

## Current State and Next Action
- state: plan-ready, nothing implemented. Baseline `4a5cce761` plus this cycle's docs artifacts (`5da9e206b`, plus the scope
  policy recorded in this commit) on `quaqua/tieude-up8044`.
- next action: `work` — start the Wave 1 phases; `P1` (`v8044-p1-gemini-schema-conjunctions`) is the natural first unit because
  it is the only phase that changes how every later schema fixture is cleaned.
