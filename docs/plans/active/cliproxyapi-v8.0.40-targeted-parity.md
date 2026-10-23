---
id: plan-20261023-v8040
type: plan
intake_id: intake-20261023-v8040
lane: high-risk
status: planned
created: 2026-10-23
updated: 2026-10-23
---

# Plan: CLIProxyAPI v8.0.40 targeted parity

> Locked from repository authority. `zharness preflight brainstorm --mode lock --json` is not part of the lifecycle in this
> repository (`AGENTS.md`: the binary is install/update/uninstall only), so the lock is recorded from the checkpoint,
> ledger, semantic review and the Lead's scope decision instead of a preflight claim.

## Outcome
- result: Port the approved v8.0.33..v8.0.40 capability slices as focused semantic ports inside existing llmhub signature,
  translator, executor, watcher, management and catalog interfaces. Scope stays additive and behavior-focused: no wholesale
  upstream merge, no branding churn, no pluginhost runtime import, no file/YAML runtime source of truth.
- success_signals:
  - Every accepted requirement cites at least one upstream `{tag} {sha}` and lands with package-level tests.
  - Postgres remains the authoritative runtime store; management/auth writes continue to persist to Postgres.
  - Amp/Kiro/Gemini CLI/Codex/Claude route behavior is unchanged except where a requirement says otherwise; the local xAI
    executor stays monolithic.
  - The perf slice is proven byte-preserving, not just "tests pass".

## Authority and Requirements
- authority:
  - `docs/upstream/cliproxyapi-checkpoint.json` — checkpoint `v8.0.40` at `d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683` /
    `refs/upstream-checkpoints/cliproxyapi/v8.0.40`; published `2026-10-23T00:10:37Z`; scope strategy `targeted-semantic-ports`.
  - `docs/upstream/cliproxyapi-gap-v8.0.33..v8.0.40.json` — 344 changed paths (match 1, baseline 3, upstream-add-absent 74,
    diverged-absent 112, upstream-delete-present-local 0, semantic-review 154). Derived and gitignored; regenerate with
    `upstream_gap.py`.
  - `docs/upstream/cliproxyapi-ledger-v8.0.33..v8.0.40.md` — 91 non-merge commits across 7 releases; every row carries a
    disposition (`adapt` 74, `defer` 8, `reject` 9).
  - `docs/upstream/cliproxyapi-semantic-review-v8.0.33..v8.0.40.md` — 154 semantic-review paths, 28 capability slices, per-slice
    local citations.
  - `~/quaqua/runs/tieude-up8040.md` — the triage proposal this plan consumes.
  - The Lead's scope decision (2026-10-23, relayed in this cycle's brief) — include 14 slices, exclude 2, defer 6, 2 conditional
    awaiting owner sign-off, 3 carried pins with no phase.
  - `CLAUDE.md` / `AGENTS.md` — Postgres-authoritative runtime; no new `web/**/*_test.go`; public SDK changes additive only;
    local xAI executor stays monolithic; llmhub branding retained; pluginhost is a stub.
  - `docs/plans/active/cliproxyapi-v8.0.33-targeted-parity.md` — prior cycle; R1-R24 landed and merged (master `64d471cb8`,
    PR #23); its four carried pins are handled in P14 or listed under Non-goals.
  - local git baseline: `64d471cb8` on `quaqua/tieude-up8040` (`quaqua/llmhub-integ` tip).
- requirements:
  - R1 [accepted]: validate and replay Meta Muse reasoning-signature envelopes, gating replay on the resolved target provider so a
    signature minted for one provider is never replayed to another. | source: `v8.0.34 7a0b7563e9a2`, `v8.0.34 674dbe8fd1be`
  - R2 [accepted]: strip the OpenRouter private router suffix from reasoning signatures on official and non-OpenRouter upstreams
    while keeping the suffix when the target really is OpenRouter. | source: `v8.0.34 a68c7d8ed7c2`, `v8.0.34 be9fbd942929`,
    `v8.0.34 cc5682c563dc`
  - R3 [accepted]: carry Gemini thought signatures through the Gemini, Interactions and OpenAI-chat protocols SDK-safely and
    order-preservingly. | source: `v8.0.34 a4660260f894`, `v8.0.34 88ac14f38530`, `v8.0.34 772df04b30d5`,
    `v8.0.36 387063909692`
  - R4 [accepted]: inline local `$ref` references in the Gemini schema cleaners and preserve literal values instead of dropping
    them. | source: `v8.0.34 c558d3203e89`
  - R5 [accepted]: strip `readOnly`/`writeOnly` as unsupported Gemini schema keywords alongside the already-stripped metadata set.
    | source: `v8.0.35 154d5144fde6`
  - R6 [accepted]: make the translator hot paths resolve tool maps, request echoes and request metadata once per stream instead of
    per tool call, emitting byte-identical output to the current implementation. | source: `v8.0.34 030d17ed947a`,
    `v8.0.34 5de9de9ae243`, `v8.0.36 77af37955244`, `v8.0.37 3b1531453e54`, `v8.0.37 95e157775f6b`, `v8.0.37 c0e619d592a6`,
    `v8.0.37 83189e5f8ce2`, `v8.0.37 80d04e195892`, `v8.0.38 74e7ff609bfd`, `v8.0.38 f4a2d0408b1d`, `v8.0.39 ac3cd66fd3e8`,
    `v8.0.39 ad09c779989e`
  - R7 [accepted]: make auth-record field updates epoch-checked and serialized against the Postgres store version, and read
    `Handler.cfg` under `h.mu` in OAuth login and token-save paths. Upstream's file epoch is replaced by the store version.
    | source: `v8.0.34 d69962e40ca6`, `v8.0.39 8bd1795599d1`
  - R8 [accepted]: preserve known credentials when an auth file is unreadable, partially written, or disappears mid-scan, instead
    of deleting the credential. | source: `v8.0.34 7484a5216a33`, `v8.0.36 5fe203d5f3c4`, `v8.0.39 57a129964fb5`
  - R9 [accepted]: preserve runtime cooldowns across file snapshots and re-read stale/persisted auth files. | source:
    `v8.0.39 d5816d5ef694`
  - R10 [accepted]: reconcile replayed `web_search_call` history with tool declarations and advertise direct tool mode for Codex
    routes while disabling responses-lite on non-Codex routes. | source: `v8.0.34 e7d4c7316fca`, `v8.0.34 8df4e42ab9ba`
  - R11 [accepted]: preserve an explicit `parallel_tool_calls: false` and honor a standalone Claude `output_config.effort` when no
    `thinking` object is present. | source: `v8.0.38 99d1698f7c59`, `v8.0.38 009f8d7ae35f`
  - R12 [accepted]: map `refusal`, `pause_turn` and `model_context_window_exceeded` stop reasons on the Claude→OpenAI chat and
    Responses paths. | source: `v8.0.36 aadb34f9161a`, `v8.0.39 d495e24cbc39`, `v8.0.39 c2dfec28d572`
  - R13 [accepted]: preserve `tool_use` stop reasons and omitted-index parallel tool calls, map
    `completion_tokens_details.reasoning_tokens`, and return `context_length_exceeded` for Claude prompt overflow stream errors.
    | source: `v8.0.36 c449529ecd17`, `v8.0.36 7db0935a6339`, `v8.0.39 43f27e3ae3ab`
  - R14 [accepted]: measure TTFT on effective token frames, counting completed reasoning items. | source: `v8.0.36 f434192a689b`,
    `v8.0.36 9d463e39f007`
  - R15 [accepted]: keep usage accounting correct across dialects and stream endings — flat `reasoning_tokens` (SGLang), usage
    after terminal stream cancellation, empty text blocks and `cache_control` injection, `response.metadata` during bootstrap
    buffering, and the effective outbound service tier after payload rules (excluding the pluginhost/redisqueue part). | source:
    `v8.0.36 e198f3cd4c52`, `v8.0.37 b12db8c3d8b6`, `v8.0.37 df07b1e59877`, `v8.0.39 9e79552fb794`, `v8.0.39 07b5a7a1ca26`,
    `v8.0.40 b90190dbaa01`
  - R16 [accepted]: recover from expired Antigravity AI credits with a negative hint and retry path. | source:
    `v8.0.34 904ec710077e`
  - R17 [accepted]: align Antigravity Hub requests, invalidate versioned model caches, and preserve tool-call replay continuity
    with caller isolation. | source: `v8.0.36 02e9e8520405`, `v8.0.38 80aa2b1d17a6`
  - R18 [accepted]: preserve the xAI image model prefix on routing and let vendor aliases fall back under `force-model-prefix`.
    | source: `v8.0.34 45ccd9c39d34`, `v8.0.36 8a3e7622df9d`
  - R19 [accepted]: pass the downstream User-Agent through and set a default xAI User-Agent. | source: `v8.0.36 5026f36531c0`
  - R20 [accepted]: escape the request path in the access log so a crafted path cannot inject log lines. | source:
    `v8.0.34 4337f1f000ff`
  - R21 [accepted]: accept an empty home model catalog, reject home-model error envelopes, and fall back to the global registry
    for model definitions of an unknown channel. | source: `v8.0.34 4d34502f07f6`, `v8.0.34 07feedf7b760`,
    `v8.0.34 50cfd2380fc0`
  - R22 [accepted]: enable Gemini/Antigravity server-side tool invocation when built-in tools and function tools are mixed in one
    request. | source: `v8.0.40 45501610f69c`
  - R23 [accepted]: reauthorize Responses websocket turns and handle the upstream `denied` status, without importing the plugin
    auth adapter. | source: `v8.0.39 b9bb031dcbb0`
  - R24 [accepted]: collapse the two live Claude-variant normalizers into one helper and close the two low-severity test gaps
    carried from the v8.0.33 P2 stream. | source: `v8.0.29 f9ac397be288`, `v8.0.33 ad9e456d4a07`, `v8.0.29 5fc580b1d72d`
  - R25 [accepted, invariant]: Postgres remains the authoritative runtime store; management/auth writes persist to it; no feature
    reads authoritative config, credentials or state from local YAML or files. Upstream's file-epoch and file-snapshot guards are
    re-expressed as store-version checks. | source: CLAUDE.md
  - R26 [accepted, invariant]: the local xAI executor stays monolithic; port behavior, not upstream's file split.
    | source: CLAUDE.md
  - R27 [accepted, invariant]: `internal/pluginhost` stays a registration stub; no plugin runtime, `sdk/pluginapi` surface or
    plugin-model binding is imported. | source: CLAUDE.md
  - R28 [accepted, invariant]: every `sdk/**` change is additive; existing `SessionAffinitySelector`, auth consumers, API handlers
    and `WebsocketInput` callers keep compiling unchanged. | source: CLAUDE.md
  - R29 [accepted, invariant]: llmhub branding, the embedded management web layout/tokens/i18n, and the release contract stay
    local; no new `web/**/*_test.go`, no README/sponsor/assets/CI/toolchain content from upstream. | source: CLAUDE.md
  - R-gate [accepted]: the final gate re-resolves the latest stable upstream release, fetches its commit into
    `refs/upstream-checkpoints/cliproxyapi/{tag}`, refreshes the checkpoint, gap and ledger, and runs the full card test plus
    `make build`. | source: upstream checkpoint integrity
  - R-gate-followup [accepted]: if the latest stable release moved after planning, implementation must not silently widen scope;
    the ledger identifies the delta and either includes it through an approved plan refinement or pins it as an explicit
    follow-up. | source: targeted-scope policy

## Non-goals
- NG1: `pluginhost` — plugin scheduler snapshot, `host.auth.models` callback, frontend auth-plugin request body, plugin load race
  fixes (`v8.0.34 1ad175b`, `v8.0.37 4846a28`, `v8.0.40 13874f1`, `v8.0.40 a014ba0`). Invariant: `internal/pluginhost/lifecycle.go:3`
  is a type-alias registration stub, not a runtime. Trigger to pull in: a plugin-runtime initiative is approved as its own cycle.
- NG2: `branding-docs-deps-ci` — README sponsors/assets, Go 1.26.10 bump, CI Go version (`v8.0.36 3f0cef0`, `v8.0.37 ce07d18`,
  `v8.0.38 d232ee1`, `v8.0.39 633a294`, `v8.0.39 ceb045a`). Invariant NG1: llmhub branding and release contracts are not upstream's.
  Trigger to pull in: never as parity; toolchain bumps follow llmhub's own dependency policy.
- NG3: `vertex-openapi-routing` — `/endpoints/openapi` route, per-model/per-credential routing settings, regional hosts
  (`v8.0.34 d9451cb`, `v8.0.37 d44f868`, `v8.0.39 88732ad`). Deferred. Trigger to pull in: an operator needs Vertex partner/open
  models, or regional us/eu endpoint hosts are requested; ~3k lines with a new config surface.
- NG4: `claude-web-search-cache` — result caching/restoration, credential scoping, tool_use-deferred reopen
  (`v8.0.34 9d453bc`, `v8.0.35 ab9c06b`, `v8.0.36 cddc9bb`, `v8.0.39 df860eb`). Deferred. Trigger to pull in: Claude server-tool
  search round-trips lose results across credential switches in production; the Home-KV part needs an approved store design.
- NG5: `codex-remote-compaction-v2` (`v8.0.35 344e0ef`). Deferred. Trigger to pull in: Kimi or Meta Responses compaction users
  report v2 failures.
- NG6: `siwc-credential-binding` (`v8.0.35 d50a199`, `v8.0.35 ae3ee3b`, `v8.0.36 88041f0`, `v8.0.36 6bbd2cb`, `v8.0.36 bb2d8f8`,
  `v8.0.37 06fd5b8`, `v8.0.40 d946c4f`). Deferred. Trigger to pull in: SIWC credential insertion is approved as a product
  capability. Do not port `v8.0.40 d946c4f` alone: it makes SIWC request-aware and depends on machinery that does not exist locally,
  while the local R18 gate at `internal/runtime/executor/codex_websockets_duplex.go:320` keys off the same attribute.
- NG7: `home-cluster-rebalance` (`v8.0.38 befcba2`). Deferred. Trigger to pull in: multi-node `internal/home` clusters are
  deployed and planned-rebalance handoff is needed; 2732 upstream lines.
- NG8: `guardian-review-credentials` (`v8.0.38 2f6f292`). Deferred. Trigger to pull in: guardian-review routing is enabled for
  a customer.
- NG9: `claude-cloak-never` (`v8.0.34 084b9ca`, `v8.0.36 20c9095`, `v8.0.39 d2e61d1`). Conditional — clients see a different
  identity on the wire. Trigger: the owner confirms that stripping Claude Code identity on API keys under `cloak_mode=never`,
  treating `sdk-ts` as native Claude Code, and validating caller headers is wanted; awaiting owner sign-off.
- NG10: Codex 96h proactive refresh lead (`v8.0.34 feb6398`). Conditional — changes refresh cadence for existing credentials from
  the local 24h at `sdk/auth/codex.go:36-37` to 96h. Trigger: the owner signs off on the cadence change; awaiting owner sign-off.
  The rejected-token handling and request-scoped 401 rules from the same slice are not blocked by this and may be proposed
  separately.
- NG11: R12 cloak-date consumer — injecting the cloaked `currentDate` into outgoing Claude requests. Carried open, no phase:
  the retention store exists at `internal/runtime/executor/helps/claude_diagnostics.go:151`
  (`LookupClaudeSessionDatePin`) with no non-test caller. Trigger: a shipped consumer design for the injection site.
- NG12: P9 F4 store-level CAS — `internal/store/postgresstore.go:354` `SaveConfig(ctx, data)` takes no expected version, while
  `SaveRuntimeSettings(ctx, expectedRevision, …)` at `internal/store/postgres_runtime_controls.go:91` does. Carried open, no phase.
  Trigger: a second writer appears for the config row (multi-instance management writes).
- NG13: P12 `KeepTopLevelEffort` consumer — declared at `sdk/cliproxy/executor/types.go:106-108`, no consumer anywhere. Carried
  open, no phase. Trigger: an SIWC request-body rule needs to read the resolved effort (NG6).
- NG14: wholesale merge of the 344 changed paths, or direct copy of upstream files. Refactors carry no user-visible outcome; port
  the behavior, not the shape.
- NG15: new `web/**/*_test.go` files; any frontend change.
- NG16: splitting the local xAI executor into upstream's file layout.

## Scope Policy (this plan)
- source: `docs/upstream/cliproxyapi-checkpoint.json` `scope_policy`, written with
  `upstream_sync.py policy --slug cliproxyapi --include … --exclude …`.
- include: `reasoning-signature-carriage`, `gemini-schema-refs`, `translator-perf`, `mgmt-auth-concurrency`,
  `watcher-credential-preservation`, `codex-fidelity`, `claude-stop-reasons`, `usage-ttft`, `antigravity`, `small-surface`,
  `management-catalog`, `gemini-server-tools`, `responses-ws-reauth`, `carried-pins`
- exclude: `pluginhost`, `branding-docs-deps-ci`
- defer (authoritative here, see caveat): `vertex-openapi-routing`, `claude-web-search-cache`, `codex-remote-compaction-v2`,
  `siwc-credential-binding`, `home-cluster-rebalance`, `guardian-review-credentials`
- conditional (awaiting owner sign-off): `claude-cloak-never`, `codex 96h refresh lead`
- caveat: `upstream_sync.py policy` writes only `include` and `exclude`. `scope_policy.defer` in the checkpoint still holds the
  v8.0.33 set (`vertex-config-upload`, `tls-handshake-timeout`, `devin-quota`, `discovery-mdns`). This plan's Non-goals are the
  authority for deferrals until the script gains `--defer`; do not treat the checkpoint's `defer` array as current.

## Approach and Risks
- approach: spend the first phases on the shared single-writer packages (`internal/signature`, `internal/util`) because every later
  translator phase reads them, then land management/watcher durability, then the provider-area groups, then the two independent
  single-commit phases, then the carried-pin cleanup, and finish with the gate. Each phase is independently verifiable with its own
  `go test` filter; no phase needs another phase's code to compile.
- single-writer facts that fix the order:
  - `internal/util/translator.go` is written by P2 (`v8.0.34 c558d3203e89`, 18 changed lines in `internal/util/translator.go`) and by P3 (`v8.0.37 80d04e195892`). P2 before P3.
  - `internal/util/gemini_schema.go` is written only by P2; `internal/util/responses_tools.go` only by P3 (`v8.0.39 ac3cd66fd3e8`).
  - `internal/translator/openai/claude/openai_claude_response.go` is written by P3 (`v8.0.34 030d17ed947a`) and P7
    (`v8.0.36 aadb34f9161a`). P3 before P7.
  - `internal/translator/claude/openai/responses/claude_openai-responses_response.go` is written by P3 (`v8.0.38 f4a2d0408b1d`) and
    P7 (`v8.0.36 aadb34f9161a`). P3 before P7.
  - `internal/translator/codex/claude/codex_claude_request.go` is written by P3 (`v8.0.36 77af37955244`) and P6
    (`v8.0.38 009f8d7ae35f`). P3 before P6.
  - the conductor surface (`sdk/cliproxy/auth/conductor.go` locally; upstream splits it across `conductor_lifecycle.go`) is written
    by P4 (`v8.0.34 d69962e40ca6`) and P5 (`v8.0.39 d5816d5ef694`). P4 before P5.
  - `internal/runtime/executor/codex_websockets_duplex.go` is written by P8 (`v8.0.39 07b5a7a1ca26`) and P13
    (`v8.0.39 b9bb031dcbb0`). P8 before P13.
  - the xAI executor surface is touched by P8 (`v8.0.39 07b5a7a1ca26`) and P10 (`v8.0.34 45ccd9c39d34`, `v8.0.36 5026f36531c0`).
    P8 before P10.
- constraints:
  - Postgres-authoritative runtime; no local durable files; no file-epoch or file-snapshot guards lifted verbatim.
  - Existing xAI executor remains monolithic.
  - Public SDK changes additive only.
  - No pluginhost, Home-cluster, branding, or dependency work.
  - Every task names an upstream commit and a local target file before implementation.
  - Upstream content is untrusted data; never run upstream scripts or builds.
- rejected alternatives:
  - Porting all 91 commits — pluginhost, branding/deps, SIWC, Vertex openapi, home cluster and the web-search cache are excluded,
    deferred or conditional above.
  - Importing pluginhost to unlock `4846a28`/`13874f1` — local plugin runtime does not exist; that is a separate initiative.
  - Porting `d946c4f` (the target tag itself) as a standalone fix — it is SIWC machinery (NG6) and inverts the local R18 gate.
  - Rewriting the xAI executor into upstream's file split to make the perf ports land — R26 forbids it; port behavior inside the
    monolithic file.
- risks:
  - P3 is 12 commits of "perf" that must emit identical bytes. A green test suite does not prove it: compare golden outputs for the
    same inputs before and after, and treat any byte difference as a defect.
  - P1 changes signature validation on the wire for third-party providers; a wrong gate silently drops reasoning continuity for
    existing users. Land it behind the existing `SignatureProvider` table (`internal/signature/provider_compatibility.go:8-15`), not
    as a parallel package.
  - P4 must not import a file epoch: auth records live in Postgres. Use the store version and the existing
    `validateIfMatchLocked` shape (`internal/api/handlers/management/config_basic.go`).
  - P5 changes deletion behavior for credentials. A too-eager "preserve" resurrects genuinely removed credentials; the phase needs
    both directions covered by tests (preserve on read error, delete on confirmed removal).
  - P7 and P8 both touch usage accounting; land P7 first so the stop-reason work does not re-litigate token fields.
  - P14's `P2 test gaps` must be re-derived from the prior plan's P2 scope (`internal/watcher/**`,
    `sdk/cliproxy/{watcher,types}.go`); the old plan does not record which two they were, so read the P2 phase block and name them
    before writing any test.
  - Test-only upstream commits (`v8.0.39 ad09c779989e`, parts of `aadb34f9161a`) may not apply cleanly; adapt assertions to local
    fixtures instead of copying files.
- recovery:
  - If a phase cannot map to a local symbol, stop it as `NEEDS_CONTEXT`; do not invent a subsystem.
  - If a parallel merge conflicts, resolve in the phase owner's branch; do not force both phases to continue.
  - If the build breaks after a phase, revert only that phase's files and re-run its focused tests.
  - If a requirement proves impossible without breaking R25-R29, surface it to the owner; do not silently drop it.

## Phases and Verification
- planning_status: planned
- phases:
  - phase_slug: v8040-p1-signature-carriage
    story_id: story-20261023-v8040-p1
    status: planned
    risk: medium
    goal: Validate and replay Meta Muse reasoning signatures, gate replay by target provider, and strip router suffixes on non-OpenRouter upstreams.
    upstream_sources:
      - v8.0.34 7a0b7563e9a2
      - v8.0.34 674dbe8fd1be
      - v8.0.34 a68c7d8ed7c2
      - v8.0.34 be9fbd942929
      - v8.0.34 cc5682c563dc
      - v8.0.34 a4660260f894
      - v8.0.34 88ac14f38530
      - v8.0.34 772df04b30d5
      - v8.0.36 387063909692
    depends_on: none
    requirements: R1, R2, R3
    allowed_surfaces:
      - `internal/signature/provider_compatibility.go`, `gpt_validation.go`, `grok_validation.go` (+ tests)
      - `internal/cache/signature_cache.go`
      - `internal/runtime/executor/openai_responses_signature.go` (+ test)
      - `internal/translator/gemini/claude/gemini_claude_response.go`, `internal/translator/common/**` (new replay helper only)
      - `internal/translator/antigravity/claude/antigravity_claude_response.go`
      - `internal/translator/codex/**`, `internal/translator/openai/claude/openai_claude_response.go`
    avoided_surfaces:
      - creating a parallel signature package or a `meta_validation.go` sibling to the local provider table
      - `internal/pluginhost/**`, `sdk/pluginapi/**`
    tasks:
      - task: [R1] Extend `SignatureProvider` and `SignatureProviderFromModelName` (`internal/signature/provider_compatibility.go:8-15,51`) with a Meta Muse provider and a target-gated replay decision, per `7a0b7563e9a2` and `674dbe8fd1be`; keep the validation entry points in the existing package.
      - task: [R2] Add router-suffix handling (`isOpenRouterUpstreamURL`, `stripRouterSuffix` equivalents) so suffixes are stripped for official and non-OpenRouter upstreams and preserved for OpenRouter, per `a68c7d8ed7c2`, `be9fbd942929`, `cc5682c563dc`.
      - task: [R3] Carry Gemini thought signatures order-preservingly through the Gemini and Interactions paths and across OpenAI chat translations, per `a4660260f894`, `88ac14f38530`, `772df04b30d5`, `387063909692`; keep any new SDK surface additive (R28).
    checks:
      - check: `go test ./internal/signature/... ./internal/cache/... -run 'Signature|Meta|Router|Gemini' -count=1`
      - check: `go test ./internal/translator/... ./internal/runtime/executor/... -run 'Signature|Reasoning|Replay' -count=1`
  - phase_slug: v8040-p2-gemini-schema-refs
    story_id: story-20261023-v8040-p2
    status: planned
    risk: low
    goal: Inline local `$ref` in the Gemini cleaners, preserve literal values, and strip `readOnly`/`writeOnly`.
    upstream_sources:
      - v8.0.34 c558d3203e89
      - v8.0.35 154d5144fde6
    depends_on: none
    requirements: R4, R5
    allowed_surfaces:
      - `internal/util/gemini_schema.go`, `internal/util/gemini_schema_test.go`
      - `internal/util/translator.go` (the 18-line caller adjustment from `c558d3203e89` only)
    avoided_surfaces:
      - `internal/util/responses_tools.go` (P3 owns it)
      - porting the 496-line upstream file shape
    tasks:
      - task: [R4] Add local-`$ref` inlining to the cleaner with the literal-value preservation upstream ships, per `c558d3203e89`; the local `$ref` walk at `internal/util/gemini_schema.go:1592` is reachability-only today.
      - task: [R5] Add `readOnly` and `writeOnly` to the unsupported-keyword strip set at `internal/util/gemini_schema.go:1186-1187`, per `154d5144fde6`.
    checks:
      - check: `go test ./internal/util/ -run 'Gemini|Schema|Ref' -count=1`
  - phase_slug: v8040-p3-translator-perf
    story_id: story-20261023-v8040-p3
    status: planned
    risk: low
    goal: Resolve tool maps, request echoes and metadata once per stream instead of per call, emitting byte-identical output.
    upstream_sources:
      - v8.0.34 030d17ed947a
      - v8.0.34 5de9de9ae243
      - v8.0.36 77af37955244
      - v8.0.37 3b1531453e54
      - v8.0.37 95e157775f6b
      - v8.0.37 c0e619d592a6
      - v8.0.37 83189e5f8ce2
      - v8.0.37 80d04e195892
      - v8.0.38 74e7ff609bfd
      - v8.0.38 f4a2d0408b1d
      - v8.0.39 ac3cd66fd3e8
      - v8.0.39 ad09c779989e
    depends_on: v8040-p2-gemini-schema-refs
    requirements: R6
    allowed_surfaces:
      - `internal/util/translator.go` (`MapClaudeToolName` at `:285`, collision policy), `internal/util/responses_tools.go`
      - `internal/translator/openai/claude/openai_claude_response.go`, `internal/translator/openai/openai/responses/openai_openai-responses_response.go`
      - `internal/translator/codex/**`, `internal/translator/antigravity/claude/antigravity_claude_response.go`
      - `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go`, `internal/translator/gemini/openai/responses/gemini_openai-responses_response.go`, `internal/translator/gemini/claude/gemini_claude_response.go`
      - associated perf tests
    avoided_surfaces:
      - `internal/util/gemini_schema.go`
      - any change to emitted bytes
    tasks:
      - task: [R6] Hoist per-call rescans into per-stream state at `internal/util/translator.go:285-294` and the translator call sites, per `80d04e195892`, `83189e5f8ce2`, `95e157775f6b`, `74e7ff609bfd`.
      - task: [R6] Pin the request payload and resolved tool maps per stream in the Gemini/OpenAI response translators, per `5de9de9ae243`; extract the request echo in one pass, per `3b1531453e54` and `f4a2d0408b1d`.
      - task: [R6] Skip the Gemini schema cleanup when building the Responses tool reverse identity map, per `ac3cd66fd3e8`; adapt the race-detector skip from `ad09c779989e` rather than copying it.
      - task: [R6] Prove byte-preservation: capture outputs for a fixed input set before and after the change and diff them; any difference fails the phase.
    checks:
      - check: `go test ./internal/util/... ./internal/translator/... -run 'Tool|Cache|Echo|Perf|Name' -count=1`
      - check: `git diff --check`
  - phase_slug: v8040-p4-mgmt-auth-concurrency
    story_id: story-20261023-v8040-p4
    status: planned
    risk: medium
    goal: Make auth-record field updates epoch-checked against the Postgres store version and read `Handler.cfg` under `h.mu`.
    upstream_sources:
      - v8.0.34 d69962e40ca6
      - v8.0.39 8bd1795599d1
    depends_on: none
    requirements: R7
    allowed_surfaces:
      - `internal/api/handlers/management/auth_files.go`, `handler.go`, `oauth_sessions.go`, `auth_files_patch_fields_test.go`
      - `sdk/cliproxy/auth/conductor.go` (the local conductor surface; upstream splits this over `conductor_lifecycle.go`)
      - `internal/store/postgresstore.go` (read-only for the version API)
    avoided_surfaces:
      - upstream's file epoch / `SourceHash` guard
      - `SaveConfig` CAS (NG12)
    tasks:
      - task: [R7] Add a safe auth-record `Modify` that validates an expected store version under the handler lock and rejects stale writers, per `d69962e40ca6`; map upstream's epoch onto the Postgres version (R25) instead of a file epoch.
      - task: [R7] Read `Handler.cfg` under `h.mu` in the OAuth login and token-save helpers, per `8bd1795599d1`; find the unsynchronized reads and cover them with the existing handler tests.
    checks:
      - check: `go test ./internal/api/handlers/management/... ./sdk/cliproxy/auth/... -run 'Modify|Epoch|Stale|Lock|Patch' -count=1`
  - phase_slug: v8040-p5-watcher-credential-preservation
    story_id: story-20261023-v8040-p5
    status: planned
    risk: medium
    goal: Preserve known credentials when an auth file is unreadable, partially written or missing mid-scan, and keep runtime cooldowns across snapshots.
    upstream_sources:
      - v8.0.34 7484a5216a33
      - v8.0.36 5fe203d5f3c4
      - v8.0.39 57a129964fb5
      - v8.0.39 d5816d5ef694
    depends_on: v8040-p4-mgmt-auth-concurrency
    requirements: R8, R9
    allowed_surfaces:
      - `internal/watcher/watcher.go`, `clients.go`, `dispatcher.go`, `internal/watcher/synthesizer/{context,file}.go`
      - `sdk/cliproxy/watcher.go`, `sdk/cliproxy/types.go`, `sdk/cliproxy/auth/types.go`, `sdk/cliproxy/auth/conductor.go`
    avoided_surfaces:
      - changing the auth-exclusion precedence landed in the v8.0.33 cycle
      - `internal/pluginhost/**`
    tasks:
      - task: [R8] Replace the fail-closed scan abort at `internal/watcher/watcher.go:216-218` with "keep the last good snapshot" for unreadable or partially written files, per `7484a5216a33`, `5fe203d5f3c4`, `57a129964fb5`; a confirmed removal must still delete.
      - task: [R9] Carry snapshot runtime state so cooldowns survive a file snapshot and stale/persisted auth files are re-read, per `d5816d5ef694`; keep Postgres authoritative (R25).
      - task: [R9] Cover both directions in tests: preserve on read error, delete on confirmed removal.
    checks:
      - check: `go test ./internal/watcher/... ./sdk/cliproxy/... -run 'Snapshot|Unreadable|Preserve|Cooldown|Discarded' -count=1`
  - phase_slug: v8040-p6-codex-fidelity
    story_id: story-20261023-v8040-p6
    status: planned
    risk: low
    goal: Reconcile replayed web_search_call history, advertise direct tool mode, preserve explicit parallel_tool_calls false, and honor standalone effort.
    upstream_sources:
      - v8.0.34 e7d4c7316fca
      - v8.0.34 8df4e42ab9ba
      - v8.0.38 99d1698f7c59
      - v8.0.38 009f8d7ae35f
    depends_on: v8040-p3-translator-perf
    requirements: R10, R11
    allowed_surfaces:
      - `internal/runtime/executor/codex_executor.go`, `codex_executor_bootstrap.go`, `codex_websockets_executor.go`
      - `internal/client/codex/models/models.go`, `cmd/fetch_codex_models/main.go`
      - `internal/translator/codex/openai/**`, `internal/translator/codex/claude/codex_claude_request.go`
      - `internal/thinking/apply.go`
    avoided_surfaces:
      - `internal/runtime/executor/codex_websockets_duplex.go` (P8/P13 own it)
      - the `KeepTopLevelEffort` flag (NG13)
    tasks:
      - task: [R10] Reconcile replayed `web_search_call` history against tool declarations, per `e7d4c7316fca`; the responses-lite signal is read at `internal/util/codex.go:18` today.
      - task: [R10] Advertise direct tool mode and disable responses-lite on non-Codex routes, per `8df4e42ab9ba`.
      - task: [R11] Keep an explicit `parallel_tool_calls: false`, per `99d1698f7c59`, and honor `output_config.effort` when `thinking` is absent at `internal/translator/codex/claude/codex_claude_request.go:354-370` and `internal/thinking/apply.go:663-665`, per `009f8d7ae35f`.
    checks:
      - check: `go test ./internal/runtime/executor/... ./internal/translator/codex/... ./internal/thinking/... -run 'Codex|Effort|Parallel|Search' -count=1`
  - phase_slug: v8040-p7-claude-stop-reasons
    story_id: story-20261023-v8040-p7
    status: planned
    risk: low
    goal: Map refusal, pause_turn and context-window stop reasons, preserve tool_use stop reasons and omitted-index parallel tool calls, and surface prompt-overflow codes.
    upstream_sources:
      - v8.0.36 aadb34f9161a
      - v8.0.36 c449529ecd17
      - v8.0.36 7db0935a6339
      - v8.0.39 d495e24cbc39
      - v8.0.39 c2dfec28d572
      - v8.0.39 43f27e3ae3ab
    depends_on: v8040-p3-translator-perf
    requirements: R12, R13
    allowed_surfaces:
      - `internal/translator/claude/openai/{chat-completions,responses}/**`
      - `internal/translator/openai/claude/openai_claude_response.go`
      - `internal/runtime/executor/claude_executor_execute.go`, `sdk/api/handlers/openai/openai_handlers.go`
      - `sdk/api/handlers/openai/openai_responses_handlers.go` (stream error mapping only)
    avoided_surfaces:
      - the codex→claude stop-reason table at `internal/translator/codex/claude/codex_claude_response.go:434` (already correct)
      - `internal/pluginhost/**`
    tasks:
      - task: [R12] Extend the Claude→OpenAI chat mapping at `internal/translator/claude/openai/chat-completions/claude_openai_response.go:294` with `pause_turn` and `model_context_window_exceeded`, and mirror it on the Responses path, per `aadb34f9161a`, `d495e24cbc39`, `c2dfec28d572`.
      - task: [R13] Preserve `tool_use` stop reasons and omitted-index parallel tool calls, per `c449529ecd17`; map `completion_tokens_details.reasoning_tokens` on the non-stream Responses path, per `7db0935a6339`; return `context_length_exceeded` for prompt-overflow stream errors, per `43f27e3ae3ab`.
    checks:
      - check: `go test ./internal/translator/claude/... ./internal/translator/openai/... ./sdk/api/handlers/... -run 'Stop|Reason|Overflow|Context|Reasoning' -count=1`
  - phase_slug: v8040-p8-usage-ttft
    story_id: story-20261023-v8040-p8
    status: planned
    risk: medium
    goal: Make usage and TTFT accounting correct across dialects and stream endings, excluding the pluginhost/redisqueue portion.
    upstream_sources:
      - v8.0.36 f434192a689b
      - v8.0.36 9d463e39f007
      - v8.0.36 e198f3cd4c52
      - v8.0.37 b12db8c3d8b6
      - v8.0.37 df07b1e59877
      - v8.0.39 9e79552fb794
      - v8.0.39 07b5a7a1ca26
      - v8.0.40 b90190dbaa01
    depends_on: v8040-p7-claude-stop-reasons
    requirements: R14, R15
    allowed_surfaces:
      - `internal/runtime/executor/helps/{responses,claude,gemini,chat}_ttft_helpers.go`, `helps/usage_helpers.go`
      - `internal/runtime/executor/{meta,gemini,kimi,openai_compat,antigravity,xai}_executor.go` (usage fields only)
      - `internal/runtime/executor/codex_websockets_duplex.go` (usage/service-tier fields only)
      - `sdk/cliproxy/usage/manager.go`, `sdk/api/handlers/openai/openai_responses_handlers.go`
    avoided_surfaces:
      - `internal/pluginhost/**` and `internal/redisqueue/**` — the excluded half of `07b5a7a1ca26`
      - `sdk/pluginapi/types.go`
    tasks:
      - task: [R14] Count completed reasoning items toward TTFT and measure streaming TTFT on effective token frames, per `f434192a689b` and `9d463e39f007`; keep the helpers in `internal/runtime/executor/helps/`.
      - task: [R15] Parse the flat `reasoning_tokens` dialect (SGLang), preserve usage after terminal stream cancellation, and hold `response.metadata` during bootstrap buffering, per `e198f3cd4c52`, `b12db8c3d8b6`, `9e79552fb794`.
      - task: [R15] Drop empty text blocks and skip them during `cache_control` injection, per `df07b1e59877`; propagate `usage.speed` into the response service tier and report the effective outbound tier after payload rules, per `b90190dbaa01` and the non-plugin half of `07b5a7a1ca26`.
    checks:
      - check: `go test ./internal/runtime/executor/... ./sdk/cliproxy/usage/... ./sdk/api/handlers/openai/... -run 'TTFT|Usage|Reasoning|ServiceTier|Cancel' -count=1`
  - phase_slug: v8040-p9-antigravity
    story_id: story-20261023-v8040-p9
    status: planned
    risk: medium
    goal: Recover expired Antigravity credits, align Hub requests, invalidate versioned model caches, and preserve tool-call replay continuity.
    upstream_sources:
      - v8.0.34 904ec710077e
      - v8.0.36 02e9e8520405
      - v8.0.38 80aa2b1d17a6
    depends_on: v8040-p8-usage-ttft
    requirements: R16, R17
    allowed_surfaces:
      - `sdk/cliproxy/auth/antigravity_credits.go`, `sdk/cliproxy/auth/conductor_home.go`
      - `internal/runtime/executor/antigravity_executor.go`, `antigravity_reasoning_replay.go`
      - `internal/auth/antigravity/auth.go`, `internal/misc/antigravity_version.go`, `sdk/cliproxy/service.go` (antigravity model registration)
      - `internal/translator/antigravity/openai/chat-completions/**`
    avoided_surfaces:
      - `internal/translator/antigravity/openai/responses/**` (P12 owns it)
      - `internal/pluginhost/**`
    tasks:
      - task: [R16] Add expired-credits negative-hint recovery and retry on top of the existing hint store at `sdk/cliproxy/auth/antigravity_credits.go:27-75`, per `904ec710077e`.
      - task: [R17] Align Hub requests and invalidate versioned model caches, per `02e9e8520405`, extending the 6h TTL cache at `internal/misc/antigravity_version.go:24,55`. The local tree has no `sdk/cliproxy/antigravity_models.go` equivalent: verify whether a versioned-model cache exists at all, and if it does not, record that as not-applicable evidence in the story instead of inventing one.
      - task: [R17] Preserve tool-call replay continuity with caller isolation in `antigravity_reasoning_replay.go` and the chat-completions translator, per `80aa2b1d17a6`.
    checks:
      - check: `go test ./internal/runtime/executor/... ./sdk/cliproxy/... ./internal/misc/... ./internal/auth/antigravity/... -run 'Antigravity|Credits|Replay|Version' -count=1`
  - phase_slug: v8040-p10-small-surface
    story_id: story-20261023-v8040-p10
    status: planned
    risk: low
    goal: Preserve the xAI image model prefix, pass the downstream User-Agent through, and escape the access-log path.
    upstream_sources:
      - v8.0.34 45ccd9c39d34
      - v8.0.36 8a3e7622df9d
      - v8.0.36 5026f36531c0
      - v8.0.34 4337f1f000ff
    depends_on: v8040-p8-usage-ttft
    requirements: R18, R19, R20
    allowed_surfaces:
      - `internal/runtime/executor/xai_executor.go` (monolithic; `executeImages` at `:211`)
      - `sdk/api/handlers/openai/openai_images_handlers.go`
      - `internal/misc/header_utils.go`, `internal/util/header_helpers.go`
      - `internal/logging/gin_logger.go`
    avoided_surfaces:
      - splitting the xAI executor into upstream's file layout (R26)
      - `internal/pluginhost/**`
    tasks:
      - task: [R18] Preserve the image model prefix on xAI routing and let vendor aliases fall back under `force-model-prefix`, per `45ccd9c39d34` and `8a3e7622df9d`, inside the monolithic executor.
      - task: [R19] Pass the downstream User-Agent through and set a default xAI User-Agent, per `5026f36531c0`; the synthesized UA is at `internal/misc/header_utils.go:43`.
      - task: [R20] Escape the request path in `internal/logging/gin_logger.go:47,121` so a crafted path cannot inject log lines, per `4337f1f000ff`.
    checks:
      - check: `go test ./internal/logging/... ./internal/runtime/executor/... ./sdk/api/handlers/openai/... -run 'Log|Escape|UserAgent|Image|Prefix' -count=1`
  - phase_slug: v8040-p11-management-catalog
    story_id: story-20261023-v8040-p11
    status: planned
    risk: low
    goal: Accept an empty home catalog, reject home-model error envelopes, and fall back to the global registry for unknown channels.
    upstream_sources:
      - v8.0.34 4d34502f07f6
      - v8.0.34 07feedf7b760
      - v8.0.34 50cfd2380fc0
    depends_on: v8040-p4-mgmt-auth-concurrency
    requirements: R21
    allowed_surfaces:
      - `internal/api/server.go` (`handleHomeModels` at `:1151`, `decodeHomeModels` at `:1324`)
      - `internal/api/handlers/management/model_definitions.go`
      - `internal/registry/model_registry.go` (read-only use of `GetAvailableModelsByProvider` at `:1568`)
    avoided_surfaces:
      - the plugin-host branch of `50cfd2380fc0` (NG1)
      - adding catalog entries
    tasks:
      - task: [R21] Allow an empty home model catalog and reject error envelopes shaped like model lists, per `4d34502f07f6` and `07feedf7b760`.
      - task: [R21] Switch the nil check at `internal/api/handlers/management/model_definitions.go:23-26` from `models == nil` to an empty check and add the global-registry fallback with deterministic sorting, per `50cfd2380fc0`; omit the plugin-host branch.
    checks:
      - check: `go test ./internal/api/... ./internal/api/handlers/management/... ./internal/registry/... -run 'Home|Catalog|ModelDefinitions|Channel' -count=1`
  - phase_slug: v8040-p12-gemini-server-tools
    story_id: story-20261023-v8040-p12
    status: planned
    risk: low
    goal: Enable server-side tool invocation when built-in and function tools are mixed in one Gemini/Antigravity request.
    upstream_sources:
      - v8.0.40 45501610f69c
    depends_on: v8040-p3-translator-perf
    requirements: R22
    allowed_surfaces:
      - `internal/translator/gemini/openai/chat-completions/gemini_openai_request.go` (mixed `toolItems` at `:451-458`)
      - `internal/translator/gemini/openai/responses/gemini_openai-responses_request.go`
      - `internal/translator/antigravity/openai/responses/antigravity_openai-responses_request.go`
    avoided_surfaces:
      - `internal/translator/antigravity/openai/chat-completions/**` (P9 owns it)
    tasks:
      - task: [R22] Diff the upstream 5-line condition against the local mixed `toolItems` block at `internal/translator/gemini/openai/chat-completions/gemini_openai_request.go:451-458` and lift only the missing behavior; mirror it on the two responses request paths, per `45501610f69c`.
    checks:
      - check: `go test ./internal/translator/gemini/... ./internal/translator/antigravity/... -run 'Tool|Mixed|Server' -count=1`
  - phase_slug: v8040-p13-responses-ws-reauth
    story_id: story-20261023-v8040-p13
    status: planned
    risk: medium
    goal: Reauthorize Responses websocket turns and handle the upstream denied status without importing the plugin auth adapter.
    upstream_sources:
      - v8.0.39 b9bb031dcbb0
    depends_on: v8040-p8-usage-ttft
    requirements: R23
    allowed_surfaces:
      - `sdk/api/handlers/openai/openai_responses_websocket.go`, `openai_responses_handlers.go`
      - `sdk/cliproxy/executor/websocket_input.go` (hook at `:28-30`), `sdk/access/errors.go`
      - `internal/runtime/executor/codex_websockets_duplex.go`
      - `internal/api/server.go` (route wiring)
    avoided_surfaces:
      - `internal/pluginhost/adapters_auth.go` and `sdk/pluginapi/types.go` — the excluded half of `b9bb031dcbb0` (R27)
    tasks:
      - task: [R23] Reauthorize on each websocket turn using the existing `WithWebsocketAuthCheck` hook at `sdk/cliproxy/executor/websocket_input.go:28-30`, per `b9bb031dcbb0`.
      - task: [R23] Handle the upstream `denied` status and surface it as an auth error via `sdk/access/errors.go`; port the duplex reauth branch only, leaving the plugin adapter untouched.
    checks:
      - check: `go test ./sdk/api/handlers/openai/... ./sdk/access/... ./internal/runtime/executor/... -run 'Websocket|Reauth|Denied|Steering' -count=1`
  - phase_slug: v8040-p14-carried-pins
    story_id: story-20261023-v8040-p14
    status: planned
    risk: low
    goal: Collapse the duplicate Claude-variant normalizers and close the two low-severity test gaps carried from the v8.0.33 P2 stream.
    upstream_sources:
      - v8.0.29 f9ac397be288
      - v8.0.33 ad9e456d4a07
      - v8.0.29 5fc580b1d72d
    depends_on: v8040-p5-watcher-credential-preservation, v8040-p11-management-catalog
    requirements: R24
    allowed_surfaces:
      - `internal/registry/model_definitions.go` (`claudeFamilyVariantIDs` at `:617`), `internal/registry/model_registry.go` (`claudeFamilyModelIDs` at `:296`)
      - `internal/watcher/**` and `sdk/cliproxy/{watcher,types}.go` tests only
    avoided_surfaces:
      - changing catalog contents
      - `internal/pluginhost/**`
    tasks:
      - task: [R24] Collapse `claudeFamilyVariantIDs` (`internal/registry/model_definitions.go:617`) and `claudeFamilyModelIDs` (`internal/registry/model_registry.go:296`) into one helper with one set of semantics (lowercasing, base-id inclusion), and keep every call site's behavior.
      - task: [R24] Re-derive the two test gaps from the v8.0.33 plan's P2 scope (`internal/watcher/**`, `sdk/cliproxy/{watcher,types}.go`), name them in the story before writing, and add only those two tests.
    checks:
      - check: `go test ./internal/registry/... -run 'Claude|Family|Variant' -count=1`
      - check: `go test ./internal/watcher/... ./sdk/cliproxy/... -run 'Snapshot|Watcher' -count=1`
  - phase_slug: v8040-p-gate
    story_id: story-20261023-v8040-gate
    status: planned
    risk: high
    goal: Prove the cycle green and pin any newer upstream delta instead of absorbing it.
    upstream_sources:
      - checkpoint v8.0.40 d946c4fba6fc
    depends_on: v8040-p1-signature-carriage, v8040-p2-gemini-schema-refs, v8040-p3-translator-perf, v8040-p4-mgmt-auth-concurrency, v8040-p5-watcher-credential-preservation, v8040-p6-codex-fidelity, v8040-p7-claude-stop-reasons, v8040-p8-usage-ttft, v8040-p9-antigravity, v8040-p10-small-surface, v8040-p11-management-catalog, v8040-p12-gemini-server-tools, v8040-p13-responses-ws-reauth, v8040-p14-carried-pins
    requirements: R-gate, R-gate-followup, R25, R26, R27, R28, R29
    allowed_surfaces:
      - merge-conflict resolution only; no new product behavior
      - `docs/upstream/**` checkpoint, gap and ledger refresh
    avoided_surfaces:
      - silent scope growth
      - excluded, deferred and conditional slices
    tasks:
      - task: [R-gate] Run `go test ./internal/... ./sdk/... -count=1`, `make build`, and `git diff --check` on the integration tip.
      - task: [R-gate] Run `python3 .claude/skills/upstream/scripts/upstream_sync.py sync --slug cliproxyapi`, then `upstream_gap.py` and `upstream_ledger.py`; record the refreshed checkpoint and the delta.
      - task: [R-gate-followup] If a newer stable release exists, pin it as an explicit follow-up with its own ledger rows; do not port it inside this cycle without an approved plan refinement.
      - task: [R25-R29] Audit the cycle diff: Postgres authority, monolithic xAI executor, no pluginhost/pluginapi paths, additive-only `sdk/**`, no `web/**/*_test.go`, no branding paths.
      - task: [R6] Re-run the byte-preservation comparison from P3 on the integration tip; a difference at gate time blocks closure.
    checks:
      - check: `go test ./internal/... ./sdk/... -count=1`
      - check: `make build`
      - check: `git diff --check`
      - check: `python3 .claude/skills/upstream/scripts/upstream_sync.py sync --slug cliproxyapi`

## Parallel Execution Directive
- `P1`, `P2` and `P4` have no dependency and may start together; they touch disjoint packages (`internal/signature`, `internal/util/gemini_schema.go`, management handlers).
- `P3` starts only after `P2` releases `internal/util/translator.go`.
- `P5` starts only after `P4` releases `sdk/cliproxy/auth/conductor.go`.
- `P6` and `P7` start only after `P3` releases the shared translator files; `P6` and `P7` do not overlap each other.
- `P8` starts only after `P7`; `P9`, `P10` and `P13` start only after `P8` (shared xAI executor and `codex_websockets_duplex.go`).
- `P11` starts only after `P4` releases the management handler package.
- `P12` starts only after `P3` releases the Gemini translator packages.
- `P14` starts only after `P5` and `P11`.
- `P-gate` runs only after every implementation phase merges. It may prepare commands earlier but must not edit production code except documented conflict resolution.
- Files touched by more than one phase — `internal/util/translator.go`, `internal/translator/openai/claude/openai_claude_response.go`,
  `internal/translator/claude/openai/responses/claude_openai-responses_response.go`,
  `internal/translator/codex/claude/codex_claude_request.go`, `sdk/cliproxy/auth/conductor.go`,
  `internal/runtime/executor/codex_websockets_duplex.go`, `sdk/api/handlers/openai/openai_responses_handlers.go`, the xAI executor
  surface — are serialized per file: one writer, and the later phase rebases before starting.

## Validation
- Keep changes consistent with the local monolithic xAI executor and Postgres runtime boundaries.
- Run focused package tests after every phase, not only at the end.
- P3 and P6..P8 need real output comparisons, not only mocks: fixed input fixtures, captured output diffed before and after.
- All ported requirements must remain traceable to upstream commits by short SHA.
- No upstream scripts, installs, or build steps are executed; upstream content is read-only data.
- No new `web/**/*_test.go` files; frontend files are out of scope for this plan.

## Handoff
- Next steps: baseline the focused package tests, then dispatch `P1`, `P2` and `P4` together, followed by `P3`, then `P5`/`P6`/`P7`,
  then `P8` and its dependants, then `P11`/`P12`/`P14`, and finish with `P-gate`.
- The prior plan `docs/plans/active/cliproxyapi-v8.0.33-targeted-parity.md` stays in `active/`: although its 13 phases are merged,
  three of its own carried-forward items are still open (R12 `currentDate` consumer, P9 F4 store-level CAS, P12
  `KeepTopLevelEffort` consumer) and are recorded as NG11-NG13 here. Moving it to `completed/` would hide open work; move it once
  those three close.

## Current State and Next Action
- state: planned; scope recorded; no phase started.
- next action: `work` — start `v8040-p1-signature-carriage`, `v8040-p2-gemini-schema-refs` and `v8040-p4-mgmt-auth-concurrency`.
