---
id: plan-20261029-v8045
type: plan
intake_id: intake-20261029-v8045
lane: high-risk
status: planned
created: 2026-10-29
updated: 2026-10-29
---

# Plan: CLIProxyAPI v8.0.45 targeted parity

> Locked from repository authority. `zharness preflight brainstorm --mode lock --json` is not part of the lifecycle in this
> repository (`AGENTS.md`: the binary is install/update/uninstall only), so the lock is recorded from the checkpoint, ledger,
> semantic review and the Lead's scope decision instead of a preflight claim.

## Outcome
- result: Port six approved v8.0.44..v8.0.45 capability slices as focused semantic ports inside the existing llmhub signature,
  thinking, logging-middleware, api-connection, codex-websocket and model-registration interfaces. Scope stays additive and
  behaviour-focused: no wholesale upstream merge, no file/YAML runtime source of truth, no pluginhost runtime import, no xAI or
  Claude executor split, no interactions translator family.
- success_signals:
  - Every behaviour requirement cites at least one upstream `{tag} {sha}` and lands with a package-level test.
  - Two security advisories are closed with a regression test that fails on the pre-fix tree: GHSA-4w5v-m7vx-jpmh (zstd window
    and expansion bounded in request logging) and the clean-apply half of GHSA-j4xx-x47j-7h54 (accept-loop `EMFILE`/`ENFILE`
    retry, `CloseWrite`, unauthenticated RESP setup deadline, HTTP header read deadline).
  - A Vertex bare-Tink thought signature survives a Vertex replay and is replaced with the bypass sentinel on every non-Vertex
    Gemini endpoint: both directions proven, not only "tests pass".
  - Postgres remains the authoritative runtime store; the local xAI and Claude executors stay monolithic; `internal/pluginhost`
    stays a stub; public `sdk/**` changes are additive only; llmhub branding is untouched; no new `web/**/*_test.go`.
  - Nothing on the defer list lands as a side effect of a phase (R14), including the two owner-gated slices.

## Authority and Requirements
- authority:
  - `docs/upstream/cliproxyapi-checkpoint.json` — checkpoint `v8.0.45` at `ad8bdd3f20bef44f0684b8a6096a216b2c76040d` /
    `refs/upstream-checkpoints/cliproxyapi/v8.0.45`; published `2026-10-27T15:27:00Z`; scope strategy
    `targeted-semantic-ports`, include 6 / exclude 0 / defer 27 (include and exclude written with `upstream_sync.py policy`,
    `exclude: []` and `defer` hand-maintained — see Scope Policy).
  - `docs/upstream/cliproxyapi-gap-v8.0.44..v8.0.45.json` — 123 changed paths (match 0, baseline 3, upstream-add-absent 25,
    diverged-absent 39, upstream-delete-present-local 0, semantic-review 56). Derived and gitignored; regenerate with
    `upstream_gap.py`.
  - `docs/upstream/cliproxyapi-ledger-v8.0.44..v8.0.45.md` — 18 non-merge commits in 1 release; every row carries a disposition
    (`adapt` 16, `defer` 2, `reject` 0).
  - `docs/upstream/cliproxyapi-semantic-review-v8.0.44..v8.0.45.md` — 56 semantic-review paths (adapt 56 / defer 0 / reject 0),
    17 capability slices with a size class per slice, the deferred-item trigger ledger, the carried-pin table and the
    just-merged-phase reopen table.
  - `~/quaqua/runs/tieude-up8045.md` — the triage proposal, trigger verdicts and the recommended first wave this plan consumes.
  - The Lead's scope decision (2026-10-29, relayed in this cycle's brief) — 6 implementation phases P1-P6 plus `P-gate`;
    `exclude: []` (this range has no pluginhost/branding/test-churn tail); defer 10 v8.0.45 slices plus the prior cycle's open
    deferrals and the two owner-gated slices.
  - `CLAUDE.md` / `AGENTS.md` — Postgres-authoritative runtime; no new `web/**/*_test.go`; public SDK changes additive only; local
    xAI executor stays monolithic; llmhub branding retained; pluginhost is a stub.
  - `docs/plans/completed/cliproxyapi-v8.0.44-targeted-parity.md` — prior cycle; 14 phases landed and merged, its carried pins
    are NG15-NG18 here.
  - local git baseline: `f49f36545` on `quaqua/llmhub-integ` (the v8.0.45 docs basis `2c6382509` merged into the integration tip).
- requirements:
  - R1 [accepted]: a prefixed Gemini model keeps its configured channel prefix in the *display name* as well as the id — the
    clone produced by `applyModelPrefixes` rewrites `Name` through `rewriteModelInfoName(name, baseID, clone.ID)` instead of
    carrying the unprefixed name. | source: `v8.0.45 908e95d94b61`
  - R2 [accepted]: request-log capture of a zstd-compressed body is bounded in both directions — the decoder is pinned to
    concurrency 1 with an 8 MiB maximum window (an oversized declared window falls back to logging the raw bytes) and the decoded
    output is capped at `max-request-body-bytes`, or 32 MiB when no smaller limit is configured; the unbounded decode helpers are
    removed. | source: `v8.0.45 ad8bdd3f20be` (GHSA-4w5v-m7vx-jpmh)
  - R3 [accepted]: deleting an absent alias field from a JSON body does not copy the document — `util.DeleteJSONBytesIfExists`
    checks the path with the existing no-copy getter and returns the input unchanged when the path is missing, and the summary
    and Gemini/Antigravity thinking appliers use it at their defensive delete sites. Output is byte-identical. | source:
    `v8.0.45 6b56ce6c28b8`
  - R4 [accepted]: when an upstream Codex websocket returns an early `usage_limit_reached` 429 before any downstream payload
    chunk is emitted, the downstream websocket is not notified of the disconnect and the conductor retries another credential on
    the same connection; a transient capacity 429 whose bootstrap time budget is exhausted is still delivered in-stream.
    | source: `v8.0.45 86a1aabf0178` (#7051)
  - R5 [accepted]: a Vertex bare-Tink Gemini thought signature (decoded byte 0 = `0x01`, four-byte key id, at least a 16-byte
    authentication tag, no protobuf wrapper) is classified as a replay-safe Gemini native signature instead of falling to unknown
    and being rewritten to the bypass sentinel, and the recognition is pinned to the key ids observed on the Vertex serve path so
    an unrelated `0x01` blob is not claimed. | source: `v8.0.45 58a4d04f3291`, `v8.0.45 a8582fbaca90`
  - R6 [accepted]: the same bare-Tink signature is treated as *incompatible* on every non-Vertex Gemini endpoint — Antigravity
    (cloudcode-pa), the Gemini API and AI Studio — where the first `functionCall` of a group receives the bypass sentinel and
    sibling parts drop the signature, while the Vertex executor keeps preserving it. R6 is the guard that keeps R5 from
    regressing: without it, R5 makes Vertex-issued history replayable verbatim onto endpoints that reject it with 400 "Corrupted
    thought signature". | source: `v8.0.45 1398f273905b`
  - R7 [accepted, partial]: the connection-hardening half of GHSA-j4xx-x47j-7h54 lands — transient `EMFILE`/`ENFILE` accept
    errors are retried with exponential backoff bounded by `maxAcceptRetryDelay` and stop on listener close, the wrapped connection
    exposes `CloseWrite`, the sniff handoff has a bounded wait, an unauthenticated RESP connection's setup read is bounded by the
    sniff deadline and cleared after authentication, and the HTTP server sets `ReadHeaderTimeout`. The body-deferral remainder is
    NG9 and is **not** part of R7. | source: `v8.0.45 477605dc6deb` (GHSA-j4xx-x47j-7h54, partial)
  - R8 [accepted, invariant]: Postgres stays the authoritative runtime store for config, credentials and state. No phase reads
    authoritative state from YAML or files, and no phase introduces a local durable store. | source: `CLAUDE.md` (local invariant)
  - R9 [accepted, invariant]: the local xAI executor (`internal/runtime/executor/xai_executor.go`) and the local Claude executor
    (`internal/runtime/executor/claude_executor.go`) stay monolithic. Upstream file splits are ported as behaviour, never as file
    layout. | source: `CLAUDE.md` (local invariant)
  - R10 [accepted, invariant]: `internal/pluginhost` stays a type-alias registration stub; no `sdk/pluginapi` or `sdk/pluginabi`
    surface is introduced. | source: `CLAUDE.md` (local invariant)
  - R11 [accepted, invariant]: changes under `sdk/**` are additive only — no removal or signature change of an exported symbol
    without a versioned additive contract. | source: `CLAUDE.md` (local invariant)
  - R12 [accepted, invariant]: llmhub branding, README and release contracts are not upstream's and are not touched.
    | source: `CLAUDE.md` (local invariant)
  - R13 [accepted, invariant]: no new `web/**/*_test.go` files and no frontend change. | source: `CLAUDE.md` (local invariant)
  - R14 [accepted, invariant]: no deferred or owner-gated slice lands as a side effect of any phase — not
    `codex-client-model-catalog`, `codex-usage-limit-capacity`, `vertex-stream-partial-args`, `responses-ws-turn-survival`,
    `devin-apply-patch-diagnostics`, `models-catalog-on-demand-refresh`, `request-state-snapshot`, `xai-image-payload-rules`,
    `api-descriptor-exhaustion-remainder`, `interactions-gemini-sse-lines`, `antigravity-caqs-channel`, the carried-open
    deferrals, `claude-cloak-never`, or the Codex 96h refresh lead. | source: this plan's scope decision
  - R-gate [accepted]: the final gate runs `make embed && make build`, `go test ./internal/... ./sdk/... -count=1`, the
    DSN-backed store suite, `git diff --check`, the scope audit, and an upstream re-resolve that refreshes the checkpoint, gap and
    ledger. | source: upstream checkpoint integrity
  - R-gate-followup [accepted]: if the latest stable upstream release moved after planning, implementation must not silently widen
    scope; the ledger identifies the delta and either includes it through an approved plan refinement or pins it as an explicit
    follow-up. `v8.0.46` and `v8.0.47` are already pinned that way (NG19). | source: targeted-scope policy

## Non-goals
- NG1: `codex-client-model-catalog` (`v8.0.45 19398539ff69`) — data-only (+721/−147 across two JSON catalogs). Deferred as a
  *data* decision, not a port: the local catalogs diverged in both directions (`internal/registry/models/codex_client_models.json`
  1560 local lines vs 1991 upstream; `models.json` 5518 vs 5012), so the merge direction needs a decision — merge the orchestrator
  restriction fields in, never replace the blobs. Trigger to pull in: the Lead rules on merge direction, or a claude client needs
  the workspace/cloud-agent restriction entries.
- NG2: `codex-usage-limit-capacity` (`v8.0.45 9a35fa12b2f3`, #7017) — `ErrorCodeTransientRateLimit`, the `trustedRetryAfterError`
  wrapper, cooldown classification and Home-aggregated deadline precedence. 606 upstream production lines across eight upstream
  files that are one local file (`sdk/cliproxy/auth/conductor.go`, 8264 lines); no local `ErrorCodeTransientRateLimit`,
  `TransientRateLimitError` or `HasTrustedRetryDeadline` (grep 0). Trigger to pull in: its own cycle, or a production report of a
  per-turn 429 cooling a credential as quota-exhausted.
- NG3: `vertex-stream-partial-args` (`v8.0.45 643161274449`) — 1209-line `partialArgs` assembler plus three translator rewrites
  (+1745/−64); no `partialArgs` symbol locally. Trigger to pull in: Vertex streaming function-call fidelity is reported as
  user-visible, as its own initiative.
- NG4: `responses-ws-turn-survival` (`v8.0.45 f73e66ac2a0d`, #7028) — `UpstreamWebsocketTurnError` contract, queue fencing and Home
  affinity; local `sdk/cliproxy/executor/websocket.go` is 31 lines against 125. Trigger to pull in: the responses-websocket client
  socket is observed closing on an upstream Codex disconnect.
- NG5: `devin-apply-patch-diagnostics` (`v8.0.45 b4f59d7035da`, #7046) — 403-line diagnostics classifier plus a bare-patch recovery
  path. Trigger to pull in: a Devin apply_patch rejection is observed in operator logs.
- NG6: `models-catalog-on-demand-refresh` (`v8.0.45 10417e5b6b85`, #7039) — introduces a `/v8/management` route family llmhub does
  not have, plus sequence fencing on `internal/registry/model_updater.go`. Trigger to pull in: an operator needs a forced catalog
  refresh without a restart.
- NG7: `request-state-snapshot` (`v8.0.45 b74422180e86`, #7056) — usage-identity and Codex-client-detection snapshots so async paths
  stop reading `ctx.Value("gin")` (`sdk/cliproxy/auth/conductor.go:6677,6704`, `sdk/api/handlers/handlers.go:242,315,1547`). Needs
  the `capturedIdentityHolder` machinery introduced in a `sdk/cliproxy/usage/manager.go` that is 401 lines against 650. Trigger to
  pull in: its own cycle.
- NG8: `xai-image-payload-rules` (`v8.0.45 826bb167802f`, #7048) — the local xAI image path applies no payload rules at all
  (`internal/runtime/executor/xai_executor.go:217-264` posts `req.Payload` directly). Trigger to pull in: an operator needs payload
  rules on xAI image endpoints.
- NG9: `api-descriptor-exhaustion-remainder` (the body-deferral half of `v8.0.45 477605dc6deb`) — defer eager request-body reads
  and temp-file creation until after authentication or past 1 MiB, expire read deadlines, close connections that finish with
  unconsumed request bodies, and `Unwrap` on the response writers. 275 upstream lines in `internal/api/server_middleware.go`, which
  does not exist locally (the local server is monolithic), plus `internal/api/middleware/response_writer.go:633`. Detail: the
  local eager read is `internal/api/middleware/request_logging.go:155` gated by `:128`, which admits `ContentLength == -1`.
  Trigger to pull in: its own cycle, or a descriptor-exhaustion report. **R7 explicitly does not cover this**, and the gate must
  not claim GHSA-j4xx fully closed.
- NG10: `interactions-gemini-sse-lines` (`v8.0.45 be929f07fa3c`) and the `helps/gemini_interactions.go` hunk of
  `v8.0.45 a8582fbaca90` — the interactions translator family was never ported (`sdk/translator/formats.go:12` declares the format;
  `internal/translator/` has no `interactions` package). Trigger to pull in: interactions translators are adopted as their own
  initiative. Same blocker as the carried-open `interactions-protocol`.
- NG11: `antigravity-caqs-channel` (`v8.0.45 7517549a6487`) — relaxes a `ChannelID == 18` gate that lives in
  `internal/signature/claude_antigravity_validation.go`, a file llmhub never ported; the local CAQS parser records the channel at
  `internal/signature/claude_validation.go:311-323` but never gates replay on it (`:362` requires presence only). Cannot exhibit
  the bug. Trigger to pull in: the CAQS replay shape is ported.
- NG12: carried-open deferrals from the v8.0.44 plan, with their triggers re-checked this cycle and all **unmet** (see
  `cliproxyapi-semantic-review-v8.0.44..v8.0.45.md` `## Deferred-item trigger ledger`): `codex-fullchain-compression` (no
  `response-compression` knob or file), `oauth-session-proxy` / `siwc-credential-binding` (no SIWC machinery),
  `discovery-mdns` (still the single-packet `zeroconf.go`; `github.com/miekg/dns` is `// indirect` only), `interactions-protocol`,
  `devin-lightning-catalog` (no lightning aggregation), `home-cluster` (no `rebalance` symbol, no `cluster_balance.go`),
  `store-gitstore-corruption` (no `isRepositoryCorruptionError`), `vertex-openapi-routing` (`helps/vertex_openapi.go` absent),
  `claude-web-search-cache` (no cache), `codex-remote-compaction-v2` (no v2 surface), `guardian-review-credentials` (no code),
  `tls-handshake-timeout` (`TLSHandshakeTimeout` only in the proxy dialer at `sdk/proxyutil/proxy.go:124`). Each keeps its own
  trigger; none is pulled in by v8.0.44..v8.0.45.
- NG13: `claude-cloak-never` — deferred-awaiting-owner. Not approved by anh iu (2026-10-26). The v8.0.44..v8.0.45 range does not
  touch the cloak path (`git diff --name-only v8.0.44 v8.0.45 | grep -i cloak` = no match). Never a phase until the owner confirms
  that stripping Claude Code identity on API keys under `cloak_mode=never` is wanted.
- NG14: Codex 96h proactive refresh lead (`codex-auth-refresh`) — deferred-awaiting-owner. `sdk/auth/codex.go:36-37` still returns
  `24 * time.Hour`, and `sdk/auth/codex.go` is not in this range's diff. Never a phase until the owner signs off on the cadence.
- NG15: carried-open pin R12 — injection of the cloaked `currentDate` into outgoing Claude requests. The retention store exists at
  `internal/runtime/executor/helps/claude_diagnostics.go:151` (`LookupClaudeSessionDatePin`) with no non-test caller (only its own
  helper `ClaudeSessionDatePinKey` at `:138` and internal retention uses at `:155`/`:199`). No phase. Trigger: a shipped consumer
  design for the injection site.
- NG16: carried-open pin P9 F4 — store-level CAS for config. `internal/store/postgresstore.go:360` `SaveConfig(ctx, data)` takes no
  expected version, while `SaveRuntimeSettings(ctx, expectedRevision, …)` at `internal/store/postgres_runtime_controls.go:91` does.
  No phase. Trigger: a second writer appears for the config row (multi-instance management writes).
- NG17: carried-open pin P12 — `KeepTopLevelEffort` consumer. Declared at `sdk/cliproxy/executor/types.go:106-108`, no consumer
  (grep 1 = the declaration). No phase. Note for the gate audit: this file is *not* written by any phase in this cycle (P4's
  allowed surface excludes it), so the pin citation stays valid. Trigger: an SIWC request-body rule needs the resolved effort
  (`siwc-credential-binding`).
- NG18: carried-open pin P13 — disable-fence durability. `disabledFileObservations` is in-memory only
  (`sdk/cliproxy/auth/conductor.go:396`); on a manager restart the map is empty, the first file snapshot is refused and becomes the
  baseline, so the fence degrades to graceful convergence rather than a durable marker. No phase — no phase in this cycle writes
  `sdk/cliproxy/auth/**`. Trigger: a durable marker is wanted across restarts (needs `Auth` plus the store's `authRuntime` field).
- NG19: v8.0.46 and v8.0.47 absorption — `v8.0.46` published `2026-10-28T11:01:00Z`, `v8.0.47` published `2026-10-29T00:27:13Z`
  (`gh release list --repo router-for-me/CLIProxyAPI --limit 3`), both newer than this checkpoint. They are the next cycle's basis,
  pinned here; this cycle does not port them and does not advance the checkpoint past `v8.0.45`.
- NG20: wholesale merge of the 123 changed paths, or direct copy of upstream files. Refactors carry no user-visible outcome; port
  the behaviour, not the shape.
- NG21: splitting the local xAI executor, the local Claude executor, `internal/api/server.go`, `sdk/api/handlers/handlers.go` or
  `sdk/cliproxy/auth/conductor.go` into upstream's file layout to make a port land.
- NG22: new `web/**/*_test.go` files; any frontend change.
- NG23: bounding `internal/logging/request_logger.go:1567` `decompressZstd` (the file-logger *response* decompression). Considered
  and excluded on purpose: `v8.0.45 ad8bdd3f20be` does not touch it and GHSA-4w5v-m7vx-jpmh scopes the request-log path only; a
  response body is produced by the upstream, not by the client, so the client-controlled-window hazard does not apply. Trigger to
  pull in: an operator feeds a hostile upstream, or a memory report points at the file logger.

## Scope Policy (this plan)
- source: `docs/upstream/cliproxyapi-checkpoint.json` `scope_policy`, written with
  `python3 .agents/skills/upstream/scripts/upstream_sync.py policy --slug cliproxyapi --strategy targeted-semantic-ports
  --include models-channel-prefix-names logging-zstd-bounds thinking-nocopy-delete codex-ws-early-429-failover
  vertex-gemini-signature-envelope api-descriptor-exhaustion`, then `exclude: []` and `defer` hand-maintained (the script only
  writes a non-empty `--exclude`, so an empty list cannot be expressed through the CLI — see the caveat below).
- include (6): `models-channel-prefix-names`, `logging-zstd-bounds`, `thinking-nocopy-delete`, `codex-ws-early-429-failover`,
  `vertex-gemini-signature-envelope`, `api-descriptor-exhaustion`
- exclude (0): `[]` — **stated explicitly**: this range has no `pluginhost`, `branding-docs` or `test-churn` tail. All 18 commits
  are behaviour (`adapt` 16, `defer` 2, `reject` 0 in the ledger), so nothing is excluded on invariant grounds this cycle. The
  previous cycle's three exclusions are not carried forward because no path in this range touches those surfaces.
- defer (27): `codex-client-model-catalog`, `codex-usage-limit-capacity`, `vertex-stream-partial-args`,
  `responses-ws-turn-survival`, `devin-apply-patch-diagnostics`, `models-catalog-on-demand-refresh`, `request-state-snapshot`,
  `xai-image-payload-rules`, `api-descriptor-exhaustion-remainder`, `interactions-gemini-sse-lines`, `antigravity-caqs-channel`,
  `codex-fullchain-compression`, `oauth-session-proxy`, `discovery-mdns`, `interactions-protocol`, `devin-lightning-catalog`,
  `home-cluster`, `store-gitstore-corruption`, `vertex-openapi-routing`, `claude-web-search-cache`, `codex-remote-compaction-v2`,
  `siwc-credential-binding`, `guardian-review-credentials`, `tls-handshake-timeout`, `claude-cloak-never`, `codex-auth-refresh`,
  `v8.0.46-v8.0.47-absorption`
- caveat: `scope_policy.defer` and `scope_policy.exclude = []` are maintained by hand because `upstream_sync.py policy` has no
  `--defer` flag and skips an empty `--exclude` (`upstream_sync.py:243-262` writes `strategy`/`include`/`exclude` only when
  non-empty, and the printed JSON echoes the previous `defer` list verbatim). The trigger text lives in NG1-NG19, not in the
  checkpoint.
- authority order: if this plan and the recorded policy disagree, rerun `policy`; do not edit the plan to match.

## Phases and Verification
- planning_status: planned
- phases:
  - phase_slug: v8045-p1-models-channel-prefix-names
    story_id: story-20261029-v8045-p1
    status: planned
    risk: low
    goal: A prefixed Gemini model's display name carries the configured channel prefix, so list and detail lookups match what a client configured.
    upstream_sources:
      - v8.0.45 908e95d94b61
    depends_on: none
    requirements: R1
    allowed_surfaces:
      - `sdk/cliproxy/service.go` (`applyModelPrefixes` at `:1974`, `rewriteModelInfoName` at `:2317`)
      - `sdk/cliproxy/config_model_display_name_test.go` and `sdk/cliproxy/model_registration_fence_test.go` (the natural hosts; there is no local `service_test.go` or `service_models.go`)
    avoided_surfaces:
      - `internal/registry/**` — the behaviour does **not** live there: upstream's file is the split `sdk/cliproxy/service_models.go`,
        absent locally, and the local function is `sdk/cliproxy/service.go:1974`
      - the other `rewriteModelInfoName` caller at `sdk/cliproxy/service.go:2448` (different function, already correct)
      - any `internal/registry/models/*.json` edit (NG1)
    tasks:
      - task: [R1] In `applyModelPrefixes`, after `clone.MetadataModelID = baseID` (`sdk/cliproxy/service.go:2013-2015`), rewrite a non-empty `clone.Name` through `rewriteModelInfoName(clone.Name, baseID, clone.ID)`, per `908e95d94b61`.
      - task: [R1] Cover the prefixed list and detail paths: a configured prefix appears in the Gemini model list response and in the model-detail lookup, and an unprefixed model is unchanged.
    checks:
      - check: `go test ./sdk/cliproxy/ -run 'Prefix|Model|Alias' -count=1`

  - phase_slug: v8045-p2-logging-zstd-bounds
    story_id: story-20261029-v8045-p2
    status: planned
    risk: low
    goal: Close GHSA-4w5v-m7vx-jpmh — the request-log zstd decode cannot be driven into an unbounded allocation by a small attacker-supplied body, in window or in output.
    upstream_sources:
      - v8.0.45 ad8bdd3f20be
    depends_on: none
    requirements: R2
    allowed_surfaces:
      - `internal/api/middleware/request_logging.go` (the zstd decode at `:213-227` and its caller `captureRequestInfoBody`)
      - `internal/api/middleware/request_logging_test.go` (extend the existing `TestCaptureRequestInfoDecodesZstdRequestBodyForLog` at `:208`)
    avoided_surfaces:
      - `internal/api/**` — **resolved unknown**: upstream's commit touches only `internal/api/middleware/request_logging.go`
        and its test, so P2 does **not** need `internal/api` and does not overlap P6's files (different package)
      - `internal/logging/request_logger.go:1567` `decompressZstd` (NG23: response path, not in the advisory)
      - the eager-read deferral at `:155` (NG9, the deferred half of P6)
    tasks:
      - task: [R2] Pin the decoder: `zstd.NewReader(..., zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(maxCapturedZstdWindowBytes))` with `maxCapturedZstdWindowBytes = 8 << 20`, so a frame declaring an oversized window fails rather than allocating, per `ad8bdd3f20be`.
      - task: [R2] Bound the output: decode through the existing limited reader path so the captured body is capped at `max-request-body-bytes`, defaulting to `maxDeferredErrorRequestBodyBytes` (32 MiB) when unset or larger, per `ad8bdd3f20be`.
      - task: [R2] Delete the now-unbounded private helpers (`decodeCapturedRequestBody`, `decodeCapturedZstdRequestBody`) and route `captureRequestInfoBody` through the single bounded function, keeping the "fall back to logging raw bytes" behaviour on decode error, per `ad8bdd3f20be`.
      - task: [R2] Regression tests that fail on the pre-fix tree: a window bomb (single-segment frame declaring an oversized window), an expansion case with no configured limit, and a normal body that still decodes.
    checks:
      - check: `go test ./internal/api/middleware/ -run 'Zstd|RequestLog|Body' -count=1`

  - phase_slug: v8045-p3-thinking-nocopy-delete
    story_id: story-20261029-v8045-p3
    status: planned
    risk: low
    goal: Delete the absent alias spellings without copying the whole body, so the thinking appliers stop allocating several full-body copies per request.
    upstream_sources:
      - v8.0.45 6b56ce6c28b8
    depends_on: none
    requirements: R3
    allowed_surfaces:
      - `internal/util/sjson_delete.go` (+ `internal/util/sjson_delete_test.go`) — the new helper
      - `internal/thinking/summary.go` (deletes at `:219`, `:228`, `:242`, `:248`, `:250`)
      - `internal/thinking/provider/gemini/apply.go`, `internal/thinking/provider/antigravity/apply.go`
      - the matching `*_test.go` files under `internal/thinking/`
    avoided_surfaces:
      - `internal/util/gjson.go` (the no-copy getter already exists at `:11`)
      - any in-place sjson write (`ReplaceInPlace`/`Optimistic`): `internal/util/nocopy_invariant_test.go:17-30` keeps
        `inPlaceSJSONAllowlist` empty precisely because no-copy results are alive at these sites — do not add the new file to it
      - the same canonicalization the thinking providers perform (output must be byte-identical)
    tasks:
      - task: [R3] Add `DeleteJSONBytesIfExists(data []byte, path string) ([]byte, error)` to `internal/util/sjson_delete.go`: return `data` unchanged when `GetGJSONBytesNoCopy(data, path)` does not exist, else `sjson.DeleteBytes`, per `6b56ce6c28b8`.
      - task: [R3] Switch the defensive alias deletes in `internal/thinking/summary.go` and the Gemini/Antigravity level+budget appliers to the new helper, per `6b56ce6c28b8`.
      - task: [R3] Prove output equivalence: the same body through both helpers yields identical bytes for present, absent and nested-absent paths; a benchmark or allocation assertion shows the absent-path copy is gone.
    checks:
      - check: `go test ./internal/util/ -run 'SJSON|Delete|NoCopy|NoCopyInvariant' -count=1`
      - check: `go test ./internal/thinking/... -run 'Summary|Level|Budget|Thinking' -count=1`

  - phase_slug: v8045-p4-codex-ws-early-429-failover
    story_id: story-20261029-v8045-p4
    status: planned
    risk: medium
    goal: An early upstream Codex websocket usage-limit 429 fails over to another credential on the same downstream connection instead of closing the client's socket.
    upstream_sources:
      - v8.0.45 86a1aabf0178
    depends_on: none
    requirements: R4
    allowed_surfaces:
      - `internal/runtime/executor/codex_websockets_executor.go` (`ExecuteStream` at `:631`; `invalidateUpstreamConnWithoutDisconnectNotify` at `:2213`)
      - `internal/runtime/executor/codex_executor.go` (`isCodexUsageLimitError` at `:1668`, read-only unless the predicate needs a companion)
      - `internal/runtime/executor/codex_websockets_*_test.go` (the local suite)
    avoided_surfaces:
      - `sdk/cliproxy/executor/**` — upstream `86a1aabf0178` touches no `sdk` file (the `executor/websocket.go` turn-error contract
        belongs to the deferred `responses-ws-turn-survival`, NG4)
      - `sdk/api/handlers/openai/openai_responses_websocket.go` (the handler half belongs to NG4's commit, not this one)
      - splitting the local executor into upstream's `codex_websockets_stream.go`/`_connection.go`/`_session.go` layout
    tasks:
      - task: [R4] Extend the bootstrap failover predicate to include the usage-limit rejection: `failoverPending = isCodexOverloadBootstrapFailure(terminalBody) || isCodexUsageLimitError(terminalBody)`, still cancelled when the bootstrap time budget is exhausted, per `86a1aabf0178`.
      - task: [R4] Suppress the downstream disconnect notification on a pre-output upstream error: notify only when `!failoverPending && len(bufferedChunks) > 0`, using `invalidateUpstreamConnWithoutDisconnectNotify` otherwise, per `86a1aabf0178`.
      - task: [R4] Close an ephemeral session on `upstream_error` and on `replay_clear_error` so the retry does not inherit a dead socket, per `86a1aabf0178`.
      - task: [R4] Regression test: a first credential returning an early `usage_limit_reached` frame before any payload chunk leaves the downstream socket open and the second credential's stream delivered; the same rejection after a buffered chunk still closes.
    checks:
      - check: `go test ./internal/runtime/executor/ -run 'CodexWebsocket|Bootstrap|UsageLimit' -count=1`

  - phase_slug: v8045-p5-vertex-gemini-signature-envelope
    story_id: story-20261029-v8045-p5
    status: planned
    risk: medium
    goal: A Vertex bare-Tink Gemini thought signature is recognised as replay-safe and preserved on Vertex, and is replaced with the bypass sentinel on every non-Vertex Gemini endpoint.
    upstream_sources:
      - v8.0.45 58a4d04f3291
      - v8.0.45 a8582fbaca90
      - v8.0.45 1398f273905b
    depends_on: none
    requirements: R5, R6
    allowed_surfaces:
      - `internal/signature/provider_compatibility.go` (`isGeminiNativeSignature` at `:353-365`, `DetectSignatureProviderForBlock` at `:137`)
      - `internal/signature/gemini_sanitize.go` (`GeminiReplaySignatureOrBypass` at `:19`, `SanitizeGeminiRequestThoughtSignatures` at `:37`)
      - `internal/runtime/executor/gemini_executor.go` (sanitize calls at `:151`, `:302`, `:575`)
      - `internal/runtime/executor/gemini_vertex_executor.go` (sanitize calls at `:348`, `:485`, `:612`, `:818`, `:1012`, `:1104` — keep preserving)
      - `internal/translator/antigravity/gemini/antigravity_gemini_request.go:155` (the local Antigravity equivalent of upstream's executor-side call)
      - `internal/runtime/executor/aistudio_executor.go` (additive: see the task note)
      - `internal/signature/*_test.go`, `internal/runtime/executor/gemini_*_test.go`
    avoided_surfaces:
      - porting upstream's envelope taxonomy: `GeminiThoughtSignatureEnvelope`/`InspectGeminiThoughtSignature`/
        `classifyGeminiThoughtSignatureEnvelope` are ~400 lines against the local 262-line `internal/signature/gemini_validation.go`
        (611 at upstream `v8.0.44`) — re-express the predicate instead, do not import the taxonomy
      - `internal/signature/gemini_validation.go` (`ValidateGeminiFunctionCallPairing` only; no validator to extend)
      - `internal/signature/claude_antigravity_validation.go` (absent locally — NG11)
      - `internal/runtime/executor/xai_executor_test.go`'s cache seed (the upstream seed-14 change belongs to the same commit but is
        test-fixture churn for a fixture this repo does not share; leave it)
    tasks:
      - task: [R5] Extend `isGeminiNativeSignature` with the bare-Tink shape: decoded length ≥ 21 (`1 + 4 + 16`), `decoded[0] == 0x01`, and the four-byte key id in the observed set, returning the same "Gemini native" answer as the field-2 wrapper, per `58a4d04f3291` and `a8582fbaca90`.
      - task: [R5] Keep the floor honest: prove a 20-byte `0x01` blob, a 21-byte blob with an unobserved key id, and a Claude/Meta payload whose first byte happens to be `0x01` are **not** claimed as Gemini native (cross-family non-claim tests).
      - task: [R6] Add `SanitizeNonVertexGeminiRequestThoughtSignatures` (an endpoint-scoped option on the existing sanitizer, not a second implementation) that treats a recognised bare-Tink signature as incompatible: first `functionCall` of the group gets `skip_thought_signature_validator`, sibling parts drop the signature, per `1398f273905b`.
      - task: [R6] Switch the non-Vertex call sites: `gemini_executor.go:151,302,575` and `internal/translator/antigravity/gemini/antigravity_gemini_request.go:155`. Leave `gemini_vertex_executor.go` on the preserving sanitizer. Per `1398f273905b`.
      - task: [R6] AI Studio (additive, cite it as such): the local `aistudio_executor.go` has **zero** signature handling (`grep -n 'signature'` = 0) because upstream calls the sanitizer from a `translateRequest` seam that does not exist locally. Add the non-Vertex sanitizer to the payload build inside `Execute`/`ExecuteStream` (`:127`, `:201`). If no payload-mutation seam exists without restructuring, stop this task and record it as a follow-up under NG9-style trigger text — the R6 guard is still satisfied for the Gemini API and Antigravity paths.
      - task: [R6] Endpoint-split tests: the same Vertex-issued history replays verbatim to Vertex, and becomes sentinel-plus-dropped-siblings on the Gemini API and Antigravity paths.
    checks:
      - check: `go test ./internal/signature/ -run 'Gemini|Signature|Envelope|BareTink' -count=1`
      - check: `go test ./internal/runtime/executor/ -run 'Gemini|Vertex|Signature' -count=1`
      - check: `go test ./internal/translator/antigravity/gemini/ -run 'Signature|Sanitize' -count=1`

  - phase_slug: v8045-p6-api-descriptor-hardening
    story_id: story-20261029-v8045-p6
    status: planned
    risk: low
    goal: Close the clean-apply half of GHSA-j4xx-x47j-7h54 — a transient file-descriptor shortage no longer kills the shared listener, an unauthenticated RESP peer cannot hold a connection open indefinitely during setup, and header reads are bounded.
    upstream_sources:
      - v8.0.45 477605dc6deb
    depends_on: none
    requirements: R7
    allowed_surfaces:
      - `internal/api/protocol_multiplexer.go` (`muxSniffDeadline`, `initialAcceptRetryDelay`, `maxAcceptRetryDelay`, `isTransientAcceptError`, `waitAcceptRetry`, the accept loop) — an exact `v8.0.44` baseline, 138 lines, clean apply
      - `internal/api/buffered_conn.go` (`CloseWrite`) — an exact `v8.0.44` baseline, 32 lines, clean apply
      - `internal/api/mux_listener.go` (`putWithTimeout`, `isClosed`, plus the `mu`/`closed`/`inFlight` fields the local struct lacks) — adapt
      - `internal/api/redis_queue_protocol.go` (`handleRedisConnection` at `:43`, reader setup at `:48`; set the deadline after it and clear it at the `authed = true` branch `:152`; `:46`/`:148` are upstream's line numbers)
      - `internal/api/server.go` (`ReadHeaderTimeout: muxSniffDeadline` in the `http.Server` literal at `internal/api/server.go:470-471` **only**; `:265` is upstream's line number, not the local one)
      - `internal/api/*_test.go` (extend `protocol_multiplexer_test.go` / `max_request_body_bytes_test.go` in place)
    avoided_surfaces:
      - `wrapUnconsumedRequestBodyHandler(engine)` — the same `server.go` hunk as `ReadHeaderTimeout`, but it is the deferred body-deferral half (NG9). Take the timeout, not the wrapper.
      - `internal/api/server_middleware.go` (absent locally — NG9)
      - `internal/api/middleware/response_writer.go` `Unwrap` and the unconsumed-body close (NG9)
      - `internal/api/response_compression.go` `Unwrap` (that file does not exist locally; llmhub has no response compression)
      - `internal/logging/cpa_trace.go` (NG9)
      - `internal/api/middleware/request_logging.go` (P2 owns it; the eager read at `:155` is NG9)
    tasks:
      - task: [R7] Add the sniff/retry constants and `isTransientAcceptError` (EMFILE/ENFILE/net.Error.Temporary, never `net.ErrClosed`) plus the bounded exponential retry in `acceptMuxConnections`, per `477605dc6deb`.
      - task: [R7] Add `muxListener.putWithTimeout`/`isClosed`: the local struct has `closeCh`/`once` but no `mu`/`closed`/`inFlight`, so add the minimal state the bounded wait needs, and make `waitAcceptRetry` stop on close, per `477605dc6deb`.
      - task: [R7] Add `bufferedConn.CloseWrite` (delegate when the wrapped conn supports it) so the wrapper stops hiding the half-close, per `477605dc6deb`.
      - task: [R7] Bound the unauthenticated RESP setup read: set the read deadline after the reader is created (`internal/api/redis_queue_protocol.go:48`) and clear it at the `authed = true` branch (`:152`), per `477605dc6deb`.
      - task: [R7] Set `ReadHeaderTimeout: muxSniffDeadline` on the `http.Server` literal in `NewServer` (`internal/api/server.go:470-471`) and **not** the unconsumed-body wrapper, per `477605dc6deb` (partial — see NG9).
      - task: [R7] Regression tests: a transient EMFILE-shaped accept error retries and does not return `net.ErrClosed`; a closed listener ends the retry promptly; an unauthenticated peer that sends nothing is dropped at the sniff deadline while an authenticated session is not.
    checks:
      - check: `go test ./internal/api/ -run 'Multiplex|Mux|Redis|Accept|Descriptor|Header' -count=1`

  - phase_slug: v8045-p-gate
    story_id: story-20261029-v8045-gate
    status: planned
    risk: medium
    goal: Prove the cycle complete: full card test, embed + build, DSN-backed store suite, clean diff, scope audit against the invariants, refreshed upstream basis, and an explicit pin for anything newer than v8.0.45.
    upstream_sources:
      - checkpoint v8.0.45 ad8bdd3f20be
    depends_on: v8045-p1-models-channel-prefix-names, v8045-p2-logging-zstd-bounds, v8045-p3-thinking-nocopy-delete, v8045-p4-codex-ws-early-429-failover, v8045-p5-vertex-gemini-signature-envelope, v8045-p6-api-descriptor-hardening
    requirements: R-gate, R-gate-followup
    allowed_surfaces:
      - `docs/upstream/**` checkpoint, gap and ledger refresh
      - `docs/plans/active/cliproxyapi-v8.0.45-targeted-parity.md` (gate result and closure sections only)
    avoided_surfaces:
      - silent scope growth
      - the defer list (NG1-NG19), including the two owner-gated slices
      - claiming GHSA-j4xx fully closed: R7 is the clean-apply half only (NG9)
    tasks:
      - task: [R-gate] Run `make embed && make build`, `go test ./internal/... ./sdk/... -count=1`, and `git diff --check` on the integration tip.
      - task: [R-gate] Run the DSN-backed store suite (`LLMHUB_POSTGRES_TEST_DSN=… go test ./internal/store/... -count=1 -v`); if no DSN is available, record that explicitly as a skipped check with the reason — do not report it as passing.
      - task: [R-gate] Run `python3 .agents/skills/upstream/scripts/upstream_sync.py sync --slug cliproxyapi`, then `upstream_gap.py` and `upstream_ledger.py`; record the refreshed checkpoint and the delta.
      - task: [R-gate-followup] If a newer stable release exists, pin it as an explicit follow-up with its own ledger rows; do not port it inside this cycle without an approved plan refinement. `v8.0.46` and `v8.0.47` are already recorded in NG19.
      - task: [R8-R14] Audit the cycle diff: Postgres authority, monolithic xAI and Claude executors, no pluginhost/pluginapi/pluginabi paths, additive-only `sdk/**`, no `web/**/*_test.go`, no branding paths, no interactions translator files, no path from the defer list.
    checks:
      - check: `make embed && make build`
      - check: `go test ./internal/... ./sdk/... -count=1`
      - check: `LLMHUB_POSTGRES_TEST_DSN=… go test ./internal/store/... -count=1 -v`
      - check: `git diff --check`
      - check: `python3 .agents/skills/upstream/scripts/upstream_sync.py sync --slug cliproxyapi`

### Phase map (one row per phase, with the shared-file note)

Slice numbers refer to `docs/upstream/cliproxyapi-semantic-review-v8.0.44..v8.0.45.md` `## Capability slices`.

| Phase | Slices | Requirements | Depends on | Shared-file note |
| --- | --- | --- | --- | --- |
| `v8045-p1-models-channel-prefix-names` | `models-channel-prefix-names` | R1 | none | sole owner of `sdk/cliproxy/service.go` |
| `v8045-p2-logging-zstd-bounds` | `logging-zstd-bounds` | R2 | none | sole owner of `internal/api/middleware/request_logging.go`; different package from P6, so no overlap |
| `v8045-p3-thinking-nocopy-delete` | `thinking-nocopy-delete` | R3 | none | sole owner of `internal/util/sjson_delete.go` and `internal/thinking/**` |
| `v8045-p4-codex-ws-early-429-failover` | `codex-ws-early-429-failover` | R4 | none | sole owner of `codex_websockets_executor.go`; no `sdk/**` surface |
| `v8045-p5-vertex-gemini-signature-envelope` | `vertex-gemini-signature-envelope` | R5, R6 | none | sole owner of `internal/signature/**`, the Gemini/Vertex/AI Studio executors and `antigravity_gemini_request.go` |
| `v8045-p6-api-descriptor-hardening` | `api-descriptor-exhaustion` (clean half) | R7 | none | sole owner of `internal/api/{protocol_multiplexer,buffered_conn,mux_listener,redis_queue_protocol}.go` and the `ReadHeaderTimeout` hunk of `server.go:470-471` |
| `v8045-p-gate` | — | R-gate, R-gate-followup | P1-P6 | docs/upstream only |

## Parallel Execution Directive
- Phase count: the scope decision in the brief says "6 phases + gate"; the enumerated list is P1-P6 plus `P-gate`, i.e. **6
  implementation phases plus `P-gate`**. This plan follows the enumeration; nothing was added or dropped.
- **Dependency answer (resolved unknown): every phase is file-disjoint, so all six run in one wave.** The brief asked whether P5
  must serialize after P3/P4; it must not — the file sets do not intersect:

  | Phase | Files | Intersects with |
  | --- | --- | --- |
  | P1 | `sdk/cliproxy/service.go` | none |
  | P2 | `internal/api/middleware/request_logging.go` (package `middleware`) | none — P6's files are package `api` |
  | P3 | `internal/util/sjson_delete.go`, `internal/thinking/{summary,provider/gemini/apply,provider/antigravity/apply}.go` | none |
  | P4 | `internal/runtime/executor/codex_websockets_executor.go`, `codex_executor.go`, codex websocket tests | none — P5's executors are the Gemini family, not Codex |
  | P5 | `internal/signature/{provider_compatibility,gemini_sanitize}.go`, `gemini_executor.go`, `gemini_vertex_executor.go`, `aistudio_executor.go`, `antigravity_gemini_request.go` | none |
  | P6 | `internal/api/{protocol_multiplexer,buffered_conn,mux_listener,redis_queue_protocol,server}.go` | none |

- Wave 1 (all six, parallel): `P1`, `P2`, `P3`, `P4`, `P5`, `P6`.
- Wave 2: `P-gate` after every phase merges.
- Because there is no technical ordering constraint, the order below is a **risk preference, not a dependency** — the Lead may run
  all six at once, and this plan expects it:
  - `P2` and `P6` first as a pair (the two security items; both small, both bounded to one package each).
  - Then `P1` (three lines) alongside `P3` (one new helper + three call sites).
  - `P4` next (the only phase whose failure mode is client-visible socket closure, so it gets the most review attention).
  - `P5` last before the gate (medium: two behaviours that must not drift apart — R5 without R6 is a regression).
- Package-level coupling that survives the file split, stated so nobody claims a false green: `P2` and `P6` both compile into the
  `internal/api` import graph (`server.go` imports `middleware`). Neither changes an exported signature (`P2` removes only private
  helpers), so a broken P2 does not block P6's own tests — but "`./internal/api/...` green" is only meaningful once both have merged.

## Validation
- Keep changes consistent with the Postgres runtime boundary, the monolithic xAI and Claude executors, and the pluginhost stub.
- Run each phase's focused `go test` filter after that phase, not only at the end; the card test runs once, at the gate.
- R2, R4, R5 and R6 need real output comparisons, not only mocked expectations: a fixed fixture diffed before and after, and for
  R5/R6 **both directions** (Vertex preserves, non-Vertex replaces).
- R2's regression tests must fail on the pre-fix tree. A window-bound test that passes before the change is not a regression test.
- R7 is deliberately partial: the gate records GHSA-j4xx as *partially* addressed and NG9 as the open remainder. Do not let the
  gate's scope audit described as "advisory closed".
- All ported requirements stay traceable to upstream commits by short SHA.
- No upstream scripts, installs or build steps are executed; upstream content is read-only data.
- No new `web/**/*_test.go` files; frontend files are out of scope for this plan.

## Handoff
- Next steps: start Wave 1 (`P1`-`P6`, any order, all parallel); the two security phases (`P2`, `P6`) and the three-line `P1` are
  the lowest-risk first units, and `P5` should not merge before `P2`/`P6` if the gate is to be read as "security first".
- Decomposition: this file is plan-ready; `to-plan` is not required for the phase list, `work` executes one phase per unit.
- Speedups deliberately not taken (state them in the kickoff, do not let a later reader think they were missed): splitting
  `477605dc6deb` beyond the clean/deferred boundary (R7 vs NG9), porting upstream's Gemini envelope taxonomy so P5 "fits" its
  source file, folding P1/P3 into one unit because both are small, and taking `codex-client-model-catalog` as a "free" data-only
  phase.
- The prior plan `docs/plans/active/cliproxyapi-v8.0.40-targeted-parity.md` stays in `active/`: its 14 phases are merged, but three
  of its carried-forward items are still open and are recorded as NG15-NG18 here. Move it to `completed/` once those close.

## Current State and Next Action
- state: plan-ready, nothing implemented. Baseline `f49f36545` plus this cycle's docs artifacts (`2c6382509`, plus the scope
  policy recorded in this commit) on `quaqua/tieude-up8045-docs`.
- next action: `work` — start Wave 1. `P1` (`v8045-p1-models-channel-prefix-names`) is the cheapest proof that the branch and the
  check filters work; `P2` (`v8045-p2-logging-zstd-bounds`) is the cheapest security win.
