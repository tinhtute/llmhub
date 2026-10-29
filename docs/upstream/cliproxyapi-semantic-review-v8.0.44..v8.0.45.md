# Semantic-review path dispositions — cliproxyapi v8.0.44..v8.0.45

- generated_from: `docs/upstream/cliproxyapi-gap-v8.0.44..v8.0.45.json` (56 `semantic-review` paths) + `docs/upstream/cliproxyapi-ledger-v8.0.44..v8.0.45.md` (18 non-merge commits, 1 release)
- local baseline: `f526dc1089e7` (`quaqua/llmhub-integ`); upstream refs `refs/upstream-checkpoints/cliproxyapi/v8.0.44` (`1f917e77c456`) -> `refs/upstream-checkpoints/cliproxyapi/v8.0.45` (`ad8bdd3f20be`)
- scope: Phases 1-3 (sync, gap, ledger, semantic review) plus a triage proposal. No policy write, no plan, no code.
- structural class counts (verbatim from `upstream_gap.py`): `match 0`, `baseline 3`, `upstream-add-absent 25`, `diverged-absent 39`, `upstream-delete-present-local 0`, `semantic-review 56` (123 changed paths).
- reading rule applied: `git diff refs/upstream-checkpoints/cliproxyapi/v8.0.44 refs/upstream-checkpoints/cliproxyapi/v8.0.45 -- <path>` for the upstream change, then the local file for the implementing symbol; no `already-present` is claimed from a path name.
- size vocabulary (added this cycle, because the build budget is the binding constraint): **small** ≤ ~150 production lines and no new file family, **medium** ~150-400 production lines or one new helper file, **structural** a new subsystem or contract, or production lines against files with no local counterpart.

## Overview

`v8.0.45` is a hardening-and-fidelity release, not a feature release. Of 18 non-merge commits, 16 are `adapt`-shaped fixes: two are security advisories (zstd window/expansion in request logging; descriptor exhaustion from unauthenticated request bodies), one is a cost/rate-limit fix (`turn_rate_limit_exceeded` treated as transient capacity), two are signature-fidelity families (Vertex bare-Tink, Antigravity CAQS), and the rest repair silent data loss in translators, websockets, catalogs, and the xAI image path. There is no `pluginhost`, branding, or test-churn tail this range — the last range's three exclusion classes do not recur.

Two structural facts drive the triage:

1. **The local tree is behind upstream on four subsystems that this range *assumes*.** `sdk/cliproxy/auth/conductor.go` is one 8264-line file against upstream's 304-line core plus eight siblings; `sdk/cliproxy/usage/manager.go` is 401 lines against 650; `internal/signature/gemini_validation.go` is 262 against 611 at `v8.0.44`; `internal/api/server_middleware.go`, `internal/api/server_routes.go`, `internal/registry/catalog_sources.go`, `internal/runtime/executor/helps/payload_finalizer.go`, `sdk/api/handlers/handlers_context.go`, `internal/translator/common/vertex_partial_args.go` and `internal/signature/claude_antigravity_validation.go` do not exist locally at all. Every one of those absences is *pre-existing* (verifiable with `git log` on the upstream side), so a slice that "just" ports a five-line fix into one of them is really paying a re-expression tax.
2. **19 of the 42 range paths that exist locally were written by a phase merged in the last cycle.** The v8.0.44 cycle's phases P2/P3/P5/P8/P9/P10/P11/P12 all own files this range touches, so a naive `git cherry-pick`-shaped port will collide with fresh code and reopen settled behavior.

## Key Concepts

- **Capability slice** — one observable outcome, portable and testable on its own (triage rubric step 1). Slices, not commits, are what gets planned.
- **Size class** — production-line volume plus structural tax; the unit the $5 build budget is spent in.
- **Trigger** — the condition that pulls a deferred item back in. `defer` without a trigger is a silent drop.
- **Re-expression tax** — the extra lines a port costs because upstream's shape (split files, shared helpers) has no local counterpart; it is why an 8-line upstream fix can be a medium local slice.

## How It Works

1. `upstream_sync.py sync --slug cliproxyapi --tag v8.0.45` rotates the checkpoint and pins the immutable ref (it would otherwise resolve a newer release than the target, which is why `--tag` is mandatory here).
2. `upstream_gap.py` classifies all 123 changed paths structurally against HEAD.
3. `upstream_ledger.py` lists the 18 non-merge commits; dispositions are filled by reading each upstream diff and the local symbol that would carry the behavior.
4. This document groups the commits into slices, scores them V/F/B, assigns a size class, and proposes include / exclude / defer.

## Where Things Live

| Local anchor | Owns |
| --- | --- |
| `sdk/cliproxy/auth/conductor.go` (8264) | every upstream `conductor_*.go`/`errors.go`/`cooldown_*` behavior, incl. the carried P13 fence |
| `sdk/cliproxy/usage/manager.go` (401) | usage identity, `With*` context carriers |
| `internal/signature/provider_compatibility.go` (366) | signature family detection (`isGeminiNativeSignature:353`) |
| `internal/signature/gemini_sanitize.go` (287) | one replay sanitizer for all Gemini endpoints (`:37`) |
| `internal/api/middleware/request_logging.go` (240) | eager body capture (`:155`) and unbounded zstd decode (`:213`) |
| `internal/api/protocol_multiplexer.go` (138) / `mux_listener.go` (68) / `buffered_conn.go` (32) | connection sniffing, accept loop |
| `internal/runtime/executor/codex_websockets_executor.go` (630+) | codex ws `ExecuteStream`, `invalidateUpstreamConn*` |
| `sdk/cliproxy/service.go` (1974, 2317) | `applyModelPrefixes`, `rewriteModelInfoName` |
| `internal/registry/model_updater.go` (116, 145, 272) | catalog refresh (no fencing) |

## Gotchas

- `internal/api/protocol_multiplexer.go` and `internal/api/buffered_conn.go` are **exact `v8.0.44` baselines** locally, so their hunks apply cleanly; `mux_listener.go` has diverged (68 vs 104). Do not assume the same for the rest of `internal/api`.
- `internal/registry/models/models.json` is *larger* locally than upstream (5518 vs 5012) while `codex_client_models.json` is smaller (1560 vs 1991): a wholesale copy loses local catalog entries.
- The carried P12 pin (`KeepTopLevelEffort`) sits in `sdk/cliproxy/executor/types.go`, a file this range writes twice.
- `isGeminiNativeSignature` accepting only `0x0a` and field-2 wrappers means every Vertex bare-Tink signature is *currently* rewritten to the bypass sentinel locally — the #7007 defect is live here, not hypothetical.

## Capability slices (triage input)

V/F/B is value / architectural fit / blast radius per `references/triage-rubric.md` (priority = V + F − B); the number ranks slices, it is not a verdict. Size class is production-line volume + structural tax; `prod` is the upstream production (+/−) count for the slice.

| Slice | Upstream source | Local status (citation) | Disposition | Decision | Size | prod | V/F/B | Risk and why |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `models-channel-prefix-names` — rewrite the display name of a prefixed Gemini model instead of only its id | `v8.0.45 908e95d94b61` | `adapt` — `sdk/cliproxy/service.go:2013-2015` sets `clone.MetadataModelID` only; `rewriteModelInfoName` already exists at `:2317`, used at `:2448` | `adapt` | **include** | small | +3 | 2/3/1 = 4 | Lowest risk item in the range: three lines between two helpers that already exist. Named-model lookups and Gemini model listings currently show the unprefixed name. |
| `logging-zstd-bounds` — bound the captured-body zstd window and output | `v8.0.45 ad8bdd3f20be` | `adapt` — `internal/api/middleware/request_logging.go:213-227` decodes with an unbounded `io.ReadAll` and a default-window reader | `adapt` | **include** | small | +8/−57 | 3/3/1 = 5 | Security (GHSA-4w5v-m7vx-jpmh). Net −49 lines: the fix is mostly deleting the unbounded helpers. Bounded output must fall back to logging raw bytes so a rejected frame never loses the log entry. |
| `thinking-nocopy-delete` — skip the document copy when deleting an absent alias field | `v8.0.45 6b56ce6c28b8` | `adapt` — `internal/thinking/summary.go:219,228,242,248,250` and both provider appliers call `sjson.DeleteBytes`; `internal/util/gjson.go:11` supplies the needed no-copy getter; `internal/util/nocopy_invariant_test.go:17-30` already guards the invariant | `adapt` | **include** | small | +52/−28 | 2/3/1 = 4 | Perf only, output unchanged. Interacts with the local no-copy invariant test: the new helper must not opt into in-place sjson, or that test goes red — which is the point of the test. |
| `codex-ws-early-429-failover` — retry another credential on an early websocket usage-limit without closing downstream | `v8.0.45 86a1aabf0178` | `adapt` — the helpers already exist (`internal/runtime/executor/codex_websockets_executor.go:2213`, `:917`; `codex_executor.go:1668`); the delta is the usage-limit predicate + buffered-chunk guard in `ExecuteStream` at `:631` | `adapt` | **include** | small | +55/−19 | 3/2/2 = 3 | Medium risk for its size: the fix decides whether the client socket is notified, so a wrong predicate either closes a healthy client connection or leaves a dead upstream attached. Upstream's own test file is 373 lines; port the assertions, not the file split. |
| `vertex-gemini-signature-envelope` — recognize Vertex bare-Tink thought signatures, pin them to observed key ids, and refuse to replay them on non-Vertex endpoints | `v8.0.45 58a4d04f3291`, `a8582fbaca90`, `1398f273905b` | `adapt` — `internal/signature/provider_compatibility.go:353-365` accepts only `0x0a`/field-2, so bare-Tink falls to `SignatureProviderUnknown` and `internal/signature/gemini_sanitize.go:19` rewrites it to the bypass sentinel; one sanitizer serves both Vertex and non-Vertex call sites (`gemini_executor.go:151,302,575` vs `gemini_vertex_executor.go:348,485,612,818,1012,1104`) | `adapt` | **include** (narrow re-expression) | medium | +155/−39 | 3/2/2 = 3 | Highest value-per-line in the range: today every Vertex Gemini signature is discarded, breaking thinking continuity on replay. Port as a predicate in `isGeminiNativeSignature` plus an endpoint-scoped sanitizer variant — **do not** port upstream's envelope taxonomy (that is 400+ lines against a 262-line local file and belongs to its own cycle). Key-id pinning is the guard that keeps the predicate from claiming arbitrary `0x01` blobs. |
| `codex-client-model-catalog` — add workspace and cloud-agent orchestrator restrictions to the client catalogs | `v8.0.45 19398539ff69` | `adapt` — data-only; local blobs diverged in both directions (`codex_client_models.json` 1560 vs 1991, `models.json` 5518 vs 5012) | `adapt` | **include** | small (data) | json +721/−147 | 2/3/1 = 4 | No Go change. Merge entries into the local blobs; a wholesale copy silently drops local catalog rows. Cheapest slice to land and it unblocks client-side model gating. |
| `xai-image-payload-rules` — apply payload default/raw/override rules to the xAI image endpoints | `v8.0.45 826bb167802f` | `adapt` — `internal/runtime/executor/xai_executor.go:217-264` posts `req.Payload` with no payload-rule stage; no `payload_finalizer.go`, no `WithOriginalRequest`, no response-format callback (grep 0) | `adapt` | **include if budget allows** | medium | +157/−24 | 2/2/2 = 2 | Real user-visible gap (operator payload rules are silently ignored for xAI images), but it needs the rule stage *introduced* on this path plus a metadata callback for `response_format`, so it is a small feature, not a wiring fix. |
| `request-state-snapshot` — snapshot usage identity and Codex-client detection instead of re-reading gin.Context | `v8.0.45 b74422180e86` | `adapt` — async paths still read `ctx.Value("gin")` (`sdk/cliproxy/auth/conductor.go:6677,6704`, `sdk/api/handlers/handlers.go:242,315,1547`); no `WithCapturedIdentity`/holder; `usage/manager.go` 401 vs 650 | `adapt` | **defer** | medium | +213/−25 | 2/2/2 = 2 | A genuine correctness bug (reused gin.Context yields another request's key/provider in usage records), but the port introduces the holder type and rewires two carrier functions in a file 250 lines behind upstream; it wants its own cycle, not a $5 tail. |
| `api-descriptor-exhaustion` — defer unauthenticated body reads, expire read deadlines, bound setup reads, retry EMFILE accepts | `v8.0.45 477605dc6deb` | `adapt` — eager unbounded read at `internal/api/middleware/request_logging.go:155` gated by `:128` (`ContentLength <= 1 MiB`, true for chunked `-1`), registered before auth at `internal/api/server.go:365`; `protocol_multiplexer.go`/`buffered_conn.go` are exact `v8.0.44` baselines, `mux_listener.go` diverged, `server_middleware.go` absent | `adapt` | **defer, split** | structural | +649/−92 | 3/2/3 = 2 | Security (GHSA-j4xx-x47j-7h54). Split it: `9a` = the accept-retry + `CloseWrite` + RESP-setup-bound halves, which touch two exact baselines and are **small**; `9b` = the body-deferral half (275 upstream lines with no local counterpart) which is **structural** and needs the auth-ordering decision in `internal/api/server.go` made explicitly. Recommend `9a` inside the $5 wave and `9b` next cycle. |
| `codex-usage-limit-capacity` — classify `turn_rate_limit_exceeded` as transient per-model capacity and carry a trusted retry deadline end to end | `v8.0.45 9a35fa12b2f3` | `adapt` — no `ErrorCodeTransientRateLimit`/`TransientRateLimitError`/`HasTrustedRetryDeadline` (grep 0); cooldown/retry logic inside the monolithic `conductor.go:144-159`; `auth/errors.go` 356 vs 550 | `adapt` | **defer** | structural | +606/−43 | 3/2/3 = 2 | Highest absolute-value commit in the range (stops a per-turn 429 from cooling a credential as quota-exhausted), but it is 606 production lines across eight upstream files that are one local file, and it must not disturb the P13 fence or the stream-truncated class merged last cycle. Its own cycle. |
| `responses-ws-turn-survival` — keep the downstream responses websocket open across an upstream Codex disconnect | `v8.0.45 f73e66ac2a0d` | `adapt` — `sdk/cliproxy/executor/websocket.go` is 31 lines (replay-required only) vs 125; no `UpstreamWebsocketTurnError`; `codex_websockets_connection.go`/`_session.go` and `openai_responses_websocket_forward.go` absent | `adapt` | **defer** | structural | +521/−51 | 3/1/3 = 1 | A new error contract plus queue fencing across handler and executor boundaries, on files the last cycle's P12 rewrote. No local anchor for half of it. |
| `vertex-stream-partial-args` — incremental `partialArgs` assembly for Vertex streaming function calls | `v8.0.45 643161274449` | `adapt` — no `partialArgs` anywhere (grep 0); `internal/translator/common/vertex_partial_args.go` absent (1209 lines) | `adapt` | **defer** | structural | +1745/−64 | 3/2/3 = 2 | The single largest commit in the range. Real fidelity gain for Vertex function-call streaming, but it is a new 1209-line assembler plus three translator rewrites — a cycle of its own, and it lands on `gemini_claude_response.go`, which the last cycle's P2/P5 just settled. |
| `devin-apply-patch-diagnostics` — content-free diagnostics on apply_patch 502s and recovery of complete bare patches | `v8.0.45 b4f59d7035da` | `adapt` — `helps/devin_apply_patch_diagnostic.go` absent (403 lines); `helps/devin_wire.go:1224-1234` has no diagnostics field; `devin_executor.go` 2572 lines; `openai_compat_executor.go:1158-1176` `statusErr` has no `LogDiagnostic` | `adapt` | **defer** | medium | +647/−81 | 2/2/2 = 2 | Observability plus a recovery path; nothing is broken without it, and it is 403 new lines of classification logic. Trigger: a Devin apply_patch rejection is actually observed in operator logs. |
| `models-catalog-on-demand-refresh` — on-demand catalog refresh with sequence fencing and `/v8/management` endpoints | `v8.0.45 10417e5b6b85` | `adapt` — no `internal/registry/catalog_sources.go`; refresh is `internal/registry/model_updater.go:116,145,272` with no fencing; no `/v8/management` route family (grep 0) | `adapt` | **defer** | medium | +326/−21 | 2/1/3 = 0 | Introduces a management API surface llmhub does not have (invariant: management stays the local layout under `/v0/management` + Postgres-authoritative config). Trigger: an operator needs to force a catalog refresh without a restart. |
| `antigravity-caqs-channel` — stop gating CAQS replay on a fixed channel id | `v8.0.45 7517549a6487` | `defer` — `internal/signature/claude_antigravity_validation.go` absent; local CAQS parser records but never gates the channel (`internal/signature/claude_validation.go:311-323`, `:362`) | `defer` | **defer** | small | +11/−4 | 1/2/1 = 2 | Cannot exhibit the bug: the rejection gate is in a file llmhub never ported. Trigger: the CAQS replay shape is ported as its own initiative. |
| `interactions-protocol` — normalize Gemini SSE lines for the interactions converter | `v8.0.45 be929f07fa3c` (+ the `helps/gemini_interactions.go` hunk of `a8582fbaca90`) | `defer` — no local interactions family (`sdk/translator/formats.go:12` declares the format; `internal/translator/` has no `interactions` package) | `defer` | **defer** | small | +9/−1 | 1/1/3 = -1 | Fix to a subsystem llmhub never ported; adding it standalone would be dead code. Trigger unchanged from the previous cycle: interactions translators adopted as their own initiative. |

## Recommended first wave for a $5 build budget

Calibration is the v8.0.44 cycle's own telemetry (`~/quaqua/runs/telemetry.jsonl`, `repo=~/Lab/llmhub`): a small slice ran 8-13 min of builder wall time plus one review round, a medium 15-27 min, and the gate 8 min. Treat the dollar column as an inference from that pace, not a measured figure.

| Order | Slice | Size | Builder estimate | Why this order |
| --- | --- | --- | --- | --- |
| 1 | `models-channel-prefix-names` | small | ~8 min | three lines between two existing helpers; lands first and proves the branch |
| 2 | `logging-zstd-bounds` | small | ~10 min | security; net −49 lines |
| 3 | `thinking-nocopy-delete` | small | ~12 min | perf; guarded by the existing no-copy invariant test |
| 4 | `codex-client-model-catalog` | small (data) | ~8 min | data-only merge; no code review surface |
| 5 | `codex-ws-early-429-failover` | small | ~15 min | behavior on existing helpers; one review round expected |
| 6 | `vertex-gemini-signature-envelope` | medium | ~25 min | highest value-per-line; narrow re-expression, taxonomy explicitly out of scope |
| 7 | `api-descriptor-exhaustion 9a` (accept retry, `CloseWrite`, RESP setup bound) | small | ~12 min | security, clean apply on two exact baselines |
| — | gate | — | ~8 min | `make build` + the card test on the integration branch |

Total ≈ 98 min of builder time across seven slices plus a gate — roughly the $5 envelope at the observed pace. Wave 2 if budget survives: `xai-image-payload-rules` (medium, ~20 min) then `request-state-snapshot` (medium, ~25 min).

**Out of this budget by size, not by value:** `codex-usage-limit-capacity` (structural), `vertex-stream-partial-args` (structural), `responses-ws-turn-survival` (structural), `api-descriptor-exhaustion 9b` (structural), `devin-apply-patch-diagnostics` (medium), `models-catalog-on-demand-refresh` (medium). If the Lead ranks the security advisory above feature fidelity, swap items 4 and 5 for `api-descriptor-exhaustion 9b` and accept one fewer medium.

## Deferred-item trigger ledger

Every item the v8.0.44 plan deferred, plus the two owner-held items, with the trigger stated and whether local evidence meets it today. "No evidence" is a citable answer; every check below was run against `f526dc1089e7`.

| Deferred item | Trigger | Local evidence checked | Verdict |
| --- | --- | --- | --- |
| `codex-fullchain-compression` | Codex egress bandwidth/cost becomes a budget item | `grep -rn 'response-compression\|ResponseCompression' --include=*.go --include=*.yaml .` = 0; `internal/api/response_compression.go` absent; `internal/config/config.go` has no compression block | trigger **unmet** — `v8.0.45` does not touch the compression path |
| `oauth-session-proxy` / `siwc-credential-binding` | SIWC credential insertion is approved as a product capability | `grep -rn 'siwc\|SIWC' --include=*.go .` = 5 hits, all incidental: `sdk/cliproxy/executor/types.go:106-108` (the P12 comment), `internal/runtime/executor/codex_websockets_duplex.go` + its test; no `UseSIWC`, no session bind, no `ProxyURL` in `internal/api/handlers/management/oauth_sessions.go` | trigger **unmet** — one more slice queues behind it (`7517549a6487`'s CAQS machinery is unrelated) |
| `discovery-mdns` | An operator depends on LAN discovery across fragmented mDNS responses | `internal/discovery/` still holds the single-packet `zeroconf.go`; no `browse_resolver.go`/`browse_queries.go`; `github.com/miekg/dns v1.1.43` is present in `go.mod:45` only as `// indirect` (transitive), so the dependency does **not** signal an adopted resolver | trigger **unmet** |
| `interactions-protocol` | Interactions translators are adopted as their own initiative | `sdk/translator/formats.go:12` declares `FormatInteractions`; `internal/translator/` has no `interactions` package; `internal/runtime/executor/helps/gemini_interactions.go` absent | trigger **unmet** — `v8.0.45` adds one more fix (`be929f07fa3c`) plus one hunk of `a8582fbaca90` to the queue |
| `devin-lightning-catalog` | A Devin models/quota cycle is opened | `grep -rn 'lightning' internal/registry/` = one catalog row (`internal/registry/models/devin_models.json:245` `swe-1-7-lightning`); no aggregation logic in `internal/registry/devin_models.go` or `helps/devin_models.go` | trigger **unmet** — `b4f59d7035da` extends the same area and is itself deferred |
| `home-cluster` | A multi-node `internal/home` cluster is deployed | `grep -rn 'rebalance\|Rebalance' --include=*.go sdk/ internal/` = 0; `internal/home/` has no `cluster_balance.go`/`cluster_latency.go`; no `sdk/cliproxy/service_home*.go` | trigger **unmet** |
| `store-gitstore-corruption` | The local git store gains a corruption-recovery path | `grep -rn 'isRepositoryCorruptionError\|Corruption' --include=*.go internal/store/*.go` = 0 | trigger **unmet** — `internal/store/` is not in the range's diff |
| `vertex-openapi-routing` | An operator needs Vertex partner/open models | `grep -rln 'vertex_openapi\|VertexOpenAPI\|openapi' --include=*.go internal/runtime/executor/helps/` = 0 | trigger **unmet** |
| `claude-web-search-cache` | Claude server-tool search loses results across credential switches in production | `grep -rn 'web_search_cache\|WebSearchCache' --include=*.go .` = 0 | trigger **unmet** |
| `codex-remote-compaction-v2` | Kimi or Meta Responses compaction users report v2 failures | `grep -rn 'remote_compaction\|RemoteCompaction' --include=*.go .` = 0; the range touches no compaction path | trigger **unmet** |
| `tls-handshake-timeout` | Silent TLS handshake hangs are observed | `TLSHandshakeTimeout` appears only in `sdk/proxyutil/proxy.go:124` (the proxy dialer) and its tests, not in `internal/config/**` and not in `helps/utls_client.go` | trigger **unmet** |
| `devin-quota` | A Devin cycle is opened (models + quota scoping together) | `internal/registry/devin_models_updater.go` and `helps/devin_models.go` exist but carry no quota scoping (`grep 'devin_quota'` = 0) | trigger **unmet**; `b4f59d7035da` joins the queue behind it |
| `guardian-review-credentials` | Guardian-review routing is enabled for a customer | `grep -rni 'guardian' --include=*.go .` = 0 | trigger **unmet** |
| `claude-cloak-never` | **Owner-gated** — anh iu confirms stripping Claude Code identity on API keys under `claude_mode=never` is wanted | Not approved. `git diff --name-only v8.0.44 v8.0.45 \| grep -i cloak` = no match | **not touched** — stays deferred-awaiting-owner |
| `codex-auth-refresh` (96h lead) | **Owner-gated** — owner signs off on changing the refresh cadence | `sdk/auth/codex.go` is not in the `v8.0.44..v8.0.45` diff | **not touched** — stays deferred-awaiting-owner |

## Carried pins — current state

| Pin | Where it lives now | State at `f526dc1089e7` | Range interaction |
| --- | --- | --- | --- |
| R12 — `currentDate` consumer | `internal/runtime/executor/helps/claude_diagnostics.go:151` `LookupClaudeSessionDatePin` (key builder at `:140`, internal uses at `:155`, `:199`) | **unchanged, still consumerless**: `LookupClaudeSessionDatePin` has no caller outside its own file, so the pinned `currentDate` is never read on a request path | `helps/claude_diagnostics.go` is not in this range |
| P9 F4 — store CAS for config | `internal/store/postgresstore.go:360` `SaveConfig(ctx, data)` | **unchanged**: the upsert computes `version + 1` server-side with no expected-version predicate, unlike `SaveRuntimeSettings(ctx, expectedRevision, …)` at `internal/store/postgres_runtime_controls.go:91` | `internal/store/` is not in this range |
| P12 — `KeepTopLevelEffort` consumer | `sdk/cliproxy/executor/types.go:106-108` | **unchanged, still declaration-only** (`grep -rn KeepTopLevelEffort` = the comment + the field) | **`sdk/cliproxy/executor/types.go` is written twice in this range** (`826bb167802f` adds `FinalizedResponseFormatCallbackMetadataKey`, `9a35fa12b2f3` adds `TransientRateLimitError`): if either slice lands, the line numbers move and the pin citation must be refreshed |
| P13 — disable-fence durability | `sdk/cliproxy/auth/conductor.go:396` `disabledFileObservations` | **unchanged, still in-memory**: on manager restart the map is empty, the first file snapshot is refused and becomes the baseline, so the fence degrades to graceful convergence rather than a durable marker | the range writes `sdk/cliproxy/auth/{types.go,errors.go,cooldown_view.go,home_selection.go,home_concurrency.go,conductor_request_scoped_errors.go}` — all of which are `conductor.go` content locally, so a port here must not disturb the fence semantics at `:2083,2307,2322-2325,2503-2507` |

## Reopen risk — range paths owned by a just-merged phase

19 of the 42 locally-present range paths were written by a v8.0.44 phase. A slice that touches one of these must be sequenced behind or explicitly reconciled with that phase, not ported blind.

| Local path | v8.0.44 phase that wrote it | v8.0.45 slice that touches it | Prod lines |
| --- | --- | --- | --- |
| `internal/signature/gemini_sanitize.go` | P9 (`gemini-thought-signature-groups`) | `vertex-gemini-signature-envelope` | +70/−13 |
| `internal/signature/gemini_validation.go` | P9 (recorded NG12: no validator to change) | `vertex-gemini-signature-envelope` | +38/−? |
| `internal/api/protocol_multiplexer.go`, `internal/api/redis_queue_protocol.go` | P3 (`redis-resp-bounds`) | `api-descriptor-exhaustion` | +72 |
| `internal/api/server.go` | P10 (`api-cors-preflight`) | `api-descriptor-exhaustion` | +5 |
| `sdk/api/handlers/handlers.go` | P11 (`stream-cancellation-reporting`) | `request-state-snapshot`, `xai-image-payload-rules` | +5 |
| `sdk/api/handlers/openai/openai_responses_websocket.go` | P11 → P12 | `responses-ws-turn-survival` | +94 |
| `internal/runtime/executor/codex_websockets_duplex.go` | P12 (`responses-ws-principal-binding`, `boundModel:184-199`) | `responses-ws-turn-survival` | +151 |
| `internal/runtime/executor/openai_compat_executor.go` | P8 (`stream-truncated-cooldown`) | `codex-usage-limit-capacity`, `devin-apply-patch-diagnostics` | +66 |
| `internal/runtime/executor/helps/usage_helpers.go` | P8 | `request-state-snapshot` | +13 |
| `sdk/cliproxy/usage/manager.go` | P8 | `request-state-snapshot` | +179 |
| `sdk/cliproxy/auth/errors.go` | P8 | `codex-usage-limit-capacity` | +200 |
| `sdk/cliproxy/executor/types.go` | P8 + carried pin P12 | `xai-image-payload-rules`, `codex-usage-limit-capacity` | +12 |
| `internal/translator/gemini/claude/gemini_claude_response.go` | P2 + P5 | `vertex-stream-partial-args` | +206 |
| `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go` | P5 | `vertex-stream-partial-args` | +152 |
| `internal/translator/gemini/openai/responses/gemini_openai-responses_response.go` | P5 | `vertex-stream-partial-args` | +242 |
| `internal/runtime/executor/gemini_executor.go` | P5 → P8 → P9 | `vertex-gemini-signature-envelope` | +8 |
| `internal/runtime/executor/gemini_vertex_executor.go` | P5 → P8 → P9 | `vertex-gemini-signature-envelope` | +4 |
| `internal/runtime/executor/aistudio_executor.go` | P8 (`aistudio-relay-frames`) | `vertex-gemini-signature-envelope` | +2 |
| `internal/runtime/executor/antigravity_executor.go` | P5/P6 | `vertex-gemini-signature-envelope` | +2 |

Of the seven recommended-wave slices, three touch a just-merged file: `vertex-gemini-signature-envelope` (P9's sanitizer + both Gemini executors), `api-descriptor-exhaustion 9a` (P3's multiplexer), and `codex-ws-early-429-failover` (no phase, but it re-expresses an upstream file split). The remaining four are clean.

## Path table — 56 `semantic-review` paths

| Disposition | Upstream path | Slice | Upstream commits | Local evidence |
| --- | --- | --- | --- | --- |
| `adapt` | `internal/api/middleware/request_logging.go` | logging-zstd-bounds, api-descriptor-exhaustion | `v8.0.45 ad8bdd3f20be`, `v8.0.45 477605dc6deb` | `:213-227` unbounded `zstd.NewReader` + `io.ReadAll`; `:128` admits `ContentLength == -1`; `:155` eager `io.ReadAll(c.Request.Body)` registered before auth at `internal/api/server.go:365`. Upstream `ad8bdd3f20be` bounds window/output and deletes the unbounded helpers; `477605dc6deb` defers the read and expires deadlines. |
| `adapt` | `internal/api/middleware/response_writer.go` | api-descriptor-exhaustion | `v8.0.45 477605dc6deb` | Local is 633 lines vs 838 upstream; the +93 hunk is the response-finish path that closes a connection with unconsumed request bytes. No local equivalent. |
| `adapt` | `internal/api/mux_listener.go` | api-descriptor-exhaustion 9a | `v8.0.45 477605dc6deb` | Local 68 lines vs 104 at `v8.0.44` — diverged before this range; the `putWithTimeout`/`isClosed` additions have no local anchor and must be adapted to the local listener. |
| `adapt` | `internal/api/server.go` | api-descriptor-exhaustion, models-catalog-on-demand-refresh | `v8.0.45 477605dc6deb`, `v8.0.45 10417e5b6b85` | Monolithic local server (1956 lines): no `MaxBytesReader`, no body-deferral middleware (`grep` 0), and no `/v8/management` route family. Both upstream hunks are re-expressions here. |
| `adapt` | `internal/api/redis_queue_protocol.go` | api-descriptor-exhaustion 9a | `v8.0.45 477605dc6deb` | +3 upstream (bound the unauthenticated RESP setup read). Local `readRESPArray` was hardened last cycle by P3; this is the adjacent setup-read bound. |
| `adapt` | `internal/api/handlers/management/model_definitions.go` | models-catalog-on-demand-refresh | `v8.0.45 10417e5b6b85` | Local management handlers live under `internal/api/handlers/management/`; the +48 hunk registers the refresh endpoint and reports `CatalogRefreshResult`, which has no local type. |
| `adapt` | `internal/registry/models/codex_client_models.json` | codex-client-model-catalog | `v8.0.45 19398539ff69` | 1560 local vs 1991 upstream lines: merge orchestrator restrictions, never replace (local catalog rows are not a subset). |
| `adapt` | `internal/api/handlers/management/model_definitions_test.go` | models-catalog-on-demand-refresh | `v8.0.45 10417e5b6b85` | Test-only (+123): refresh-endpoint assertions. Local management handler tests exist; extend them rather than adding a file. |
| `adapt` | `sdk/api/handlers/openai/openai_images_handlers_test.go` | xai-image-payload-rules | `v8.0.45 826bb167802f` | Test-only (+294): payload default/raw/override cases for the xAI image endpoints. The local suite has `sdk/api/handlers/openai/openai_images_handlers.go` but no payload-rule coverage. |

| `adapt` | `internal/registry/models/models.json` | codex-client-model-catalog | `v8.0.45 19398539ff69` | 5518 local vs 5012 upstream lines: the local catalog is *larger*; a wholesale copy loses local entries. |
| `adapt` | `internal/runtime/executor/aistudio_executor.go` | vertex-gemini-signature-envelope | `v8.0.45 1398f273905b` | +2: switch the sanitizer call to the endpoint-scoped variant. Local has one `SanitizeGeminiRequestThoughtSignatures` and no variant. |
| `adapt` | `internal/runtime/executor/antigravity_executor.go` | vertex-gemini-signature-envelope | `v8.0.45 1398f273905b` | +2/−1: same sanitizer switch on the Antigravity path. |
| `adapt` | `internal/runtime/executor/gemini_executor.go` | vertex-gemini-signature-envelope | `v8.0.45 1398f273905b` | +8/−? — three call sites (`:151,302,575`) move to the non-Vertex sanitizer. |
| `adapt` | `internal/runtime/executor/gemini_vertex_executor.go` | vertex-gemini-signature-envelope | `v8.0.45 1398f273905b`, `a8582fbaca90` | +4 — the Vertex executor keeps the preserving sanitizer at `:348,485,612,818,1012,1104`; only the surrounding plumbing changes. |
| `adapt` | `internal/runtime/executor/codex_openai_images.go` | codex-usage-limit-capacity | `v8.0.45 9a35fa12b2f3` | +8/−? — classify the image path's 429s through the new transient-capacity contract; no local transient class exists. |
| `adapt` | `internal/runtime/executor/openai_compat_executor.go` | codex-usage-limit-capacity, devin-apply-patch-diagnostics | `v8.0.45 9a35fa12b2f3`, `b4f59d7035da` | Local 950 lines vs 1252; `statusErr` at `:1158-1176` has `streamTruncated` but no `transientRateLimit` and no `diagnostic`/`LogDiagnostic()`. |
| `adapt` | `internal/runtime/executor/devin_executor.go` | devin-apply-patch-diagnostics | `v8.0.45 b4f59d7035da` | Local 2572 lines vs 2872; the +301 hunk lands in the monolith. No diagnostics helper exists. |
| `adapt` | `internal/runtime/executor/helps/devin_wire.go` | devin-apply-patch-diagnostics | `v8.0.45 b4f59d7035da` | `:1224-1234` `DevinUpstreamResponseLog` needs the `ApplyPatchDiagnostics` field; the local struct matches upstream pre-fix exactly. |
| `adapt` | `internal/runtime/executor/helps/usage_helpers.go` | request-state-snapshot | `v8.0.45 b74422180e86` | +13/−? — usage identity snapshot. Local 1346 lines vs 1682 upstream; the carrier functions the fix rewires (`WithAPIKey`, `WithAccessProvider`) live in `sdk/cliproxy/usage/manager.go` locally. |
| `adapt` | `internal/runtime/executor/codex_websockets_duplex.go` | responses-ws-turn-survival | `v8.0.45 f73e66ac2a0d` | +151/−? — turn error fencing. Local file is the P12 owner (`boundModel:184-199`) and carries no turn-error concept. |
| `adapt` | `internal/signature/gemini_sanitize.go` | vertex-gemini-signature-envelope | `v8.0.45 1398f273905b`, `a8582fbaca90` | Local `:19` `GeminiReplaySignatureOrBypass` and `:37` the single sanitizer; no option struct, no `SanitizeNonVertexGeminiRequestThoughtSignatures`. |
| `adapt` | `internal/signature/gemini_validation.go` | vertex-gemini-signature-envelope | `v8.0.45 58a4d04f3291`, `a8582fbaca90` | Local 262 lines vs 673: no envelope taxonomy, no `InspectGeminiThoughtSignature`, no `isGeminiBareTinkEnvelope`. |
| `adapt` | `internal/signature/provider_compatibility.go` | vertex-gemini-signature-envelope | `v8.0.45 58a4d04f3291`, `a8582fbaca90` | `:353-365` `isGeminiNativeSignature` accepts `0x0a` or field-2 only — the exact hole #7007 describes. |
| `adapt` | `internal/thinking/provider/antigravity/apply.go` | thinking-nocopy-delete | `v8.0.45 6b56ce6c28b8` | Alias deletes still call `sjson.DeleteBytes`; must switch to `util.DeleteJSONBytesIfExists`. |
| `adapt` | `internal/thinking/provider/gemini/apply.go` | thinking-nocopy-delete | `v8.0.45 6b56ce6c28b8` | Same; level and budget alias deletes. |
| `adapt` | `internal/thinking/summary.go` | thinking-nocopy-delete | `v8.0.45 6b56ce6c28b8` | `:219,228,242,248,250` — the five defensive deletes named in the upstream commit message. |
| `adapt` | `internal/translator/gemini/claude/gemini_claude_response.go` | vertex-stream-partial-args | `v8.0.45 643161274449` | +206/−? — `partialArgs` streaming assembly. Local 537 lines vs 808; no `partialArgs` symbol. |
| `adapt` | `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go` | vertex-stream-partial-args | `v8.0.45 643161274449` | +152/−? — same, chat-completions surface. Local 495 lines. |
| `adapt` | `internal/translator/gemini/openai/responses/gemini_openai-responses_response.go` | vertex-stream-partial-args | `v8.0.45 643161274449` | +242/−? — same, responses surface. Local 1899 lines. |
| `adapt` | `sdk/api/handlers/handlers.go` | request-state-snapshot, xai-image-payload-rules | `v8.0.45 b74422180e86`, `826bb167802f` | Monolithic local handler (1563 lines vs 784 upstream after the split): `ctx.Value("gin")` at `:242,315,1547` is the #7056 hazard; the image-payload context carriers have no local file to live in. |
| `adapt` | `sdk/api/handlers/claude/code_handlers.go` | codex-usage-limit-capacity | `v8.0.45 9a35fa12b2f3` | +5 — surface the trusted retry deadline on the Claude error path; no `HasTrustedRetryDeadline` locally. |
| `adapt` | `sdk/api/handlers/openai/openai_images_handlers.go` | xai-image-payload-rules | `v8.0.45 826bb167802f` | +91/−? — build the un-defaulted original payload and report the finalized `response_format`. Local handler exists but has no payload-rule stage. |
| `adapt` | `sdk/api/handlers/openai/openai_responses_websocket.go` | responses-ws-turn-survival | `v8.0.45 f73e66ac2a0d` | +94/−? — keep the downstream socket open on upstream loss. Local file is the P11→P12 owner; half the upstream hunk lives in files this repo does not have (`*_forward.go`, `*_session.go`). |
| `adapt` | `sdk/api/handlers/openai/openai_responses_websocket_input.go` | responses-ws-turn-survival | `v8.0.45 f73e66ac2a0d` | −?/+7 — pre-loss queued-input fencing; local file is 2 lines short of the upstream baseline. |
| `adapt` | `sdk/cliproxy/auth/conductor_request_scoped_errors.go` | codex-usage-limit-capacity | `v8.0.45 9a35fa12b2f3`, `f73e66ac2a0d` | +3/−3 — request-scoped error plumbing for the turn/rate-limit classes; local file exists but has neither class. |
| `adapt` | `sdk/cliproxy/auth/cooldown_view.go` | codex-usage-limit-capacity | `v8.0.45 9a35fa12b2f3` | `:132-135` / `:165-170` add the `transient_rate_limit` reason on both the error and message paths; no such reason locally. |
| `adapt` | `sdk/cliproxy/auth/errors.go` | codex-usage-limit-capacity | `v8.0.45 9a35fa12b2f3` | Local 356 lines vs 550: has `ErrorCodeStreamTruncated:39` but no `ErrorCodeTransientRateLimit`, no `trustedRetryAfterError`, no `withTrustedRetryAfter`, no `HasTrustedRetryDeadline`. |
| `adapt` | `sdk/cliproxy/auth/home_concurrency.go` | codex-usage-limit-capacity | `v8.0.45 9a35fa12b2f3` | +23/−? — Home attempt bookkeeping for the trusted deadline; local file exists at the pre-fix shape. |
| `adapt` | `sdk/cliproxy/auth/home_selection.go` | codex-usage-limit-capacity, responses-ws-turn-survival | `v8.0.45 9a35fa12b2f3`, `f73e66ac2a0d` | +60/−? — Home aggregated deadline authoritative over per-attempt hints, plus pinned-affinity preservation across turn releases. |
| `adapt` | `sdk/cliproxy/auth/types.go` | request-state-snapshot | `v8.0.45 b74422180e86` | +29/−? — `WithCodexClientRequest` + `codexClientRequestContextKey`; local `IsCodexClientRequest` does not exist and Codex detection is inline. |
| `adapt` | `sdk/cliproxy/executor/types.go` | xai-image-payload-rules, codex-usage-limit-capacity | `v8.0.45 826bb167802f`, `9a35fa12b2f3` | +12 — `FinalizedResponseFormatCallbackMetadataKey` and the `TransientRateLimitError` contract. **Also holds the carried P12 pin at `:106-108`**: re-cite after either lands. |
| `adapt` | `sdk/cliproxy/executor/websocket.go` | responses-ws-turn-survival | `v8.0.45 f73e66ac2a0d` | Local 31 lines vs 125: only `UpstreamWebsocketReplayRequiredError`; no `UpstreamWebsocketTurnError` family. |
| `adapt` | `sdk/cliproxy/executor/websocket_input.go` | responses-ws-turn-survival | `v8.0.45 f73e66ac2a0d` | +2 — input fencing carrier; local file is 2 lines behind the upstream baseline. |
| `adapt` | `sdk/cliproxy/usage/manager.go` | request-state-snapshot | `v8.0.45 b74422180e86` | Local 401 lines vs 650 at `v8.0.44`: no `capturedIdentityHolder`, no `WithCapturedIdentity`; `WithAPIKey`/`WithAccessProvider` are plain `context.WithValue`. |
| `adapt` | `sdk/cliproxy/auth/home_selection_attempt_test.go` | codex-usage-limit-capacity | `v8.0.45 9a35fa12b2f3` | Test-only; the local suite keeps its own home-selection attempt tests. Port assertions, not the file. |
| `adapt` | `internal/api/middleware/request_logging_test.go` | logging-zstd-bounds | `v8.0.45 ad8bdd3f20be` | Local test already exercises zstd decode (`request_logging_test.go:208-228`); extend it with the window/expansion cases rather than adding a file. |
| `adapt` | `internal/signature/gemini_sanitize_test.go` | vertex-gemini-signature-envelope | `v8.0.45 1398f273905b` | Extend the local suite with the endpoint split (preserve on Vertex, bypass elsewhere). |
| `adapt` | `internal/signature/gemini_validation_test.go` | vertex-gemini-signature-envelope | `v8.0.45 58a4d04f3291`, `a8582fbaca90` | Bare-Tink detection, floor rejection and key-id pinning cases. |
| `adapt` | `internal/signature/provider_compatibility_test.go` | vertex-gemini-signature-envelope | `v8.0.45 a8582fbaca90` | Cross-family non-claim cases (a `0x01` blob must not become Claude/Meta). |
| `adapt` | `internal/signature/grok_validation_test.go` | vertex-gemini-signature-envelope | `v8.0.45 a8582fbaca90`, `1398f273905b` | Upstream moved the xAI cache seed off the `0x01`-colliding value; the local grok tests use the same collision-prone seed and must follow. |
| `adapt` | `internal/runtime/executor/xai_executor_test.go` | vertex-gemini-signature-envelope, xai-image-payload-rules | `v8.0.45 a8582fbaca90`, `1398f273905b`, `826bb167802f` | Three commits touch this test file; the local file exists and needs the seed change plus the image payload-rule cases. |
| `adapt` | `internal/translator/gemini/claude/gemini_claude_response_test.go` | vertex-stream-partial-args | `v8.0.45 643161274449` | +218 — partialArgs streaming assertions. |
| `adapt` | `internal/translator/gemini/openai/chat-completions/gemini_openai_response_test.go` | vertex-stream-partial-args | `v8.0.45 643161274449` | +206. |
| `adapt` | `internal/translator/gemini/openai/responses/gemini_openai-responses_response_test.go` | vertex-stream-partial-args | `v8.0.45 643161274449` | +203. |
| `adapt` | `sdk/cliproxy/service_oauth_model_alias_test.go` | models-channel-prefix-names | `v8.0.45 908e95d94b61` | Test-only (+128): prefixed-model list/detail assertions. The local suite has its own alias tests; extend them. |
| `adapt` | `sdk/api/handlers/handlers_metadata_test.go` | request-state-snapshot | `v8.0.45 b74422180e86` | Test-only (+115): metadata snapshot assertions. |

Class note: the gap script classifies test files independently of the production file they exercise, so `upstream-add-absent` test files still belong to the slice of their production counterpart -- they are listed in the grouped table below, not here. No semantic-review path is `defer` or `reject` this range: every one of the 56 carries behavior llmhub needs, and the deferrals are all slice-level (size or absent subsystem).

## Path table — `upstream-add-absent` (25), `diverged-absent` (39), `baseline` (3)

Every path below is listed individually so a reader can grep it. Disposition here is slice-level: each path inherits the
decision of the slice that owns it, which is why the `defer` count for these 67 paths is not zero even though the 56
`semantic-review` paths are all `adapt`.

### `upstream-add-absent` — production (3)

| Path | Slice | Decision | Why absent | Upstream source |
| --- | --- | --- | --- | --- |
| `internal/util/sjson_delete.go` | thinking-nocopy-delete | include | New 21-line helper; the local equivalent has to be created (its dependency `GetGJSONBytesNoCopy` already exists at `internal/util/gjson.go:11`) | `v8.0.45 6b56ce6c28b8` |
| `internal/translator/common/vertex_partial_args.go` | vertex-stream-partial-args | defer | New 1209-line incremental `partialArgs` assembler | `v8.0.45 643161274449` |
| `internal/runtime/executor/helps/devin_apply_patch_diagnostic.go` | devin-apply-patch-diagnostics | defer | New 403-line content-free diagnostics classifier | `v8.0.45 b4f59d7035da` |

### `upstream-add-absent` — tests (22)

All folded into the slice of their production counterpart; port assertions, never the file layout (`AGENTS.md`: extend existing
test cases before adding files).

| Path | Slice | Decision | Upstream source |
| --- | --- | --- | --- |
| `internal/runtime/executor/codex_turn_rate_limit_issue7017_test.go` | codex-usage-limit-capacity | defer | `v8.0.45 9a35fa12b2f3` |
| `internal/runtime/executor/codex_websocket_home_loss_test.go` | responses-ws-turn-survival | defer | `v8.0.45 f73e66ac2a0d` |
| `internal/runtime/executor/codex_websockets_write_loss_test.go` | responses-ws-turn-survival | defer | `v8.0.45 f73e66ac2a0d` |
| `internal/runtime/executor/devin_apply_patch_diagnostic_test.go` | devin-apply-patch-diagnostics | defer | `v8.0.45 b4f59d7035da` |
| `internal/runtime/executor/gemini_vertex_bare_tink_signature_test.go` | vertex-gemini-signature-envelope | include | `v8.0.45 58a4d04f3291`, `1398f273905b` |
| `internal/runtime/executor/gemini_vertex_interactions_sse_test.go` | interactions-protocol | defer | `v8.0.45 be929f07fa3c`, `a8582fbaca90` |
| `internal/runtime/executor/helps/devin_apply_patch_diagnostic_test.go` | devin-apply-patch-diagnostics | defer | `v8.0.45 b4f59d7035da` |
| `internal/runtime/executor/helps/gemini_interactions_test.go` | interactions-protocol | defer | `v8.0.45 a8582fbaca90` |
| `internal/translator/common/vertex_partial_args_test.go` | vertex-stream-partial-args | defer | `v8.0.45 643161274449` |
| `internal/translator/gemini/interactions/interactions_gemini_stream_lines_test.go` | interactions-protocol | defer | `v8.0.45 be929f07fa3c` |
| `internal/util/sjson_delete_test.go` | thinking-nocopy-delete | include | `v8.0.45 6b56ce6c28b8` |
| `sdk/api/handlers/issue7017_retry_after_test.go` | codex-usage-limit-capacity | defer | `v8.0.45 9a35fa12b2f3` |
| `sdk/api/handlers/openai/openai_responses_websocket_loss_test.go` | responses-ws-turn-survival | defer | `v8.0.45 f73e66ac2a0d` |
| `sdk/cliproxy/auth/conductor_issue7017_test.go` | codex-usage-limit-capacity | defer | `v8.0.45 9a35fa12b2f3` |
| `sdk/cliproxy/auth/conductor_issue7017_execution_test.go` | codex-usage-limit-capacity | defer | `v8.0.45 9a35fa12b2f3` |
| `sdk/cliproxy/auth/conductor_issue7017_review_r1_test.go` | codex-usage-limit-capacity | defer | `v8.0.45 9a35fa12b2f3` |
| `sdk/cliproxy/auth/conductor_issue7017_review_r3_test.go` | codex-usage-limit-capacity | defer | `v8.0.45 9a35fa12b2f3` |
| `sdk/cliproxy/auth/conductor_issue7017_review_r4_test.go` | codex-usage-limit-capacity | defer | `v8.0.45 9a35fa12b2f3` |
| `sdk/cliproxy/auth/conductor_issue7017_review_r5_test.go` | codex-usage-limit-capacity | defer | `v8.0.45 9a35fa12b2f3` |
| `sdk/cliproxy/auth/conductor_issue7017_review_r6_test.go` | codex-usage-limit-capacity | defer | `v8.0.45 9a35fa12b2f3` |
| `sdk/cliproxy/auth/conductor_issue7056_test.go` | request-state-snapshot | defer | `v8.0.45 b74422180e86` |
| `sdk/cliproxy/auth/trusted_retry_after_test.go` | codex-usage-limit-capacity | defer | `v8.0.45 9a35fa12b2f3` |

### `diverged-absent` — production (33)

No local counterpart: upstream split these out of monoliths the local tree still keeps whole, so each hunk lands elsewhere
locally or not at all. This group is the re-expression tax that drives the size classes.

| Path | Slice | Decision | Local owner of the behavior |
| --- | --- | --- | --- |
| `internal/api/response_compression.go` | — | reject | `codex-fullchain-compression` stays deferred (trigger unmet); not in this range's scope |
| `internal/api/server_management_v8.go` | models-catalog-on-demand-refresh | defer | `internal/api/server.go` (no `/v8/management` family) |
| `internal/api/server_middleware.go` | api-descriptor-exhaustion 9b | defer | `internal/api/server.go` + `internal/api/middleware/request_logging.go` |
| `internal/api/server_routes.go` | api-descriptor-exhaustion, models-catalog-on-demand-refresh | defer | `internal/api/server.go` (1956 lines, routes inline) |
| `internal/logging/cpa_trace.go` | api-descriptor-exhaustion 9b | defer | `internal/logging/request_logger.go` |
| `internal/registry/catalog_sources.go` | models-catalog-on-demand-refresh | defer | `internal/registry/model_updater.go:116,145,272` |
| `internal/runtime/executor/codex_executor_execute.go` | codex-usage-limit-capacity | defer | `internal/runtime/executor/codex_executor.go` |
| `internal/runtime/executor/codex_executor_stream.go` | codex-usage-limit-capacity | defer | `internal/runtime/executor/codex_executor.go` |
| `internal/runtime/executor/codex_executor_terminal.go` | codex-usage-limit-capacity | defer | `internal/runtime/executor/codex_executor.go` |
| `internal/runtime/executor/codex_websockets_connection.go` | responses-ws-turn-survival | defer | `internal/runtime/executor/codex_websockets_executor.go` |
| `internal/runtime/executor/codex_websockets_errors.go` | codex-usage-limit-capacity | defer | `internal/runtime/executor/codex_websockets_executor.go` (`:2213` area) |
| `internal/runtime/executor/codex_websockets_execute.go` | codex-usage-limit-capacity | defer | `internal/runtime/executor/codex_websockets_executor.go:631` |
| `internal/runtime/executor/codex_websockets_session.go` | responses-ws-turn-survival | defer | `internal/runtime/executor/codex_websockets_duplex.go` |
| `internal/runtime/executor/codex_websockets_stream.go` | codex-ws-early-429-failover, codex-usage-limit-capacity | **include** (`codex-ws-early-429-failover` hunks only) | `internal/runtime/executor/codex_websockets_executor.go:631` `ExecuteStream` |
| `internal/runtime/executor/helps/gemini_interactions.go` | interactions-protocol | defer | no local counterpart at all |
| `internal/runtime/executor/helps/payload_finalizer.go` | xai-image-payload-rules | include-if-budget | `internal/runtime/executor/helps/payload_helpers.go:28` `ApplyPayloadConfigWithRequest` — rule application exists, the finalizer/callback stage does not |
| `internal/runtime/executor/xai_executor_media.go` | xai-image-payload-rules | include-if-budget | `internal/runtime/executor/xai_executor.go:217-264` |
| `internal/signature/claude_antigravity_validation.go` | antigravity-caqs-channel | defer | `internal/signature/claude_validation.go:311-323` (channel parsed, never gated) |
| `internal/signature/signaturetest/claude.go` | antigravity-caqs-channel | defer | no local `signaturetest` package |
| `internal/translator/gemini/interactions/interactions_gemini_common.go` | interactions-protocol | defer | no local interactions translator family |
| `sdk/api/handlers/handlers_context.go` | xai-image-payload-rules, request-state-snapshot | include-if-budget | `sdk/api/handlers/handlers.go` |
| `sdk/api/handlers/handlers_errors.go` | codex-usage-limit-capacity | defer | `sdk/api/handlers/handlers.go` |
| `sdk/api/handlers/handlers_execution.go` | xai-image-payload-rules | include-if-budget | `sdk/api/handlers/handlers.go` |
| `sdk/api/handlers/handlers_stream.go` | responses-ws-turn-survival | defer | `sdk/api/handlers/handlers.go` |
| `sdk/api/handlers/openai/openai_responses_websocket_forward.go` | responses-ws-turn-survival | defer | `sdk/api/handlers/openai/openai_responses_websocket.go:1562` `forwardResponsesWebsocket` (the local file is not split) |
| `sdk/api/handlers/openai/openai_responses_websocket_session.go` | responses-ws-turn-survival | defer | `sdk/api/handlers/openai/openai_responses_websocket.go` |
| `sdk/cliproxy/auth/conductor_cooldown.go` | codex-usage-limit-capacity | defer | `sdk/cliproxy/auth/conductor.go` (8264 lines) |
| `sdk/cliproxy/auth/conductor_execution.go` | codex-usage-limit-capacity, responses-ws-turn-survival | defer | `sdk/cliproxy/auth/conductor.go` |
| `sdk/cliproxy/auth/conductor_home.go` | responses-ws-turn-survival | defer | `sdk/cliproxy/auth/conductor.go` |
| `sdk/cliproxy/auth/conductor_home_execution.go` | codex-usage-limit-capacity | defer | `sdk/cliproxy/auth/conductor.go` |
| `sdk/cliproxy/auth/conductor_selection.go` | codex-usage-limit-capacity | defer | `sdk/cliproxy/auth/conductor.go` |
| `sdk/cliproxy/auth/conductor_stream.go` | codex-usage-limit-capacity, responses-ws-turn-survival | defer | `sdk/cliproxy/auth/conductor.go` |
| `sdk/cliproxy/service_models.go` | models-channel-prefix-names | include | `sdk/cliproxy/service.go:1974` `applyModelPrefixes`, `:2317` `rewriteModelInfoName` |

### `diverged-absent` — tests (6)

| Path | Slice | Decision | Upstream source |
| --- | --- | --- | --- |
| `internal/api/max_request_body_bytes_test.go` | api-descriptor-exhaustion | defer | `v8.0.45 477605dc6deb` |
| `internal/api/server_management_v8_test.go` | models-catalog-on-demand-refresh | defer | `v8.0.45 10417e5b6b85` |
| `internal/registry/catalog_sources_test.go` | models-catalog-on-demand-refresh | defer | `v8.0.45 10417e5b6b85` |
| `internal/runtime/executor/antigravity_caqs_replay_test.go` | antigravity-caqs-channel | defer | `v8.0.45 7517549a6487` |
| `internal/runtime/executor/codex_stream_bootstrap_buffering_test.go` | codex-ws-early-429-failover | include | `v8.0.45 86a1aabf0178` |
| `internal/signature/claude_antigravity_validation_test.go` | antigravity-caqs-channel | defer | `v8.0.45 7517549a6487` |

### `baseline` — byte-identical to `v8.0.44` locally (3)

The only clean-apply surface in the range.

| Path | Slice | Decision | Evidence |
| --- | --- | --- | --- |
| `internal/api/buffered_conn.go` | api-descriptor-exhaustion 9a | include | 32 local lines = 32 at `v8.0.44`; the `CloseWrite` addition applies verbatim |
| `internal/api/protocol_multiplexer.go` | api-descriptor-exhaustion 9a | include | 138 local lines = 138 at `v8.0.44`; the accept-retry additions apply verbatim |
| `internal/api/protocol_multiplexer_test.go` | api-descriptor-exhaustion 9a | include | same baseline; port as assertions into the existing test file |

## Disposition counts (semantic-review paths)

- `adapt`: 56
- `defer`: 0
- `reject`: 0
- `already-present`: 0
- total: 56 `semantic-review` paths, of 123 changed paths

Slice-level dispositions differ from the path counts: 10 slices `include`, 7 `defer` (of which `antigravity-caqs-channel` and
`interactions-protocol` are deferrals of paths the gap script classes `diverged-absent`, not `semantic-review`).

## Open questions for the Lead

1. **Is `api-descriptor-exhaustion 9b` inside or outside the $5 wave?** It is the only admissible security item the recommended wave does not cover in full; `9a` covers the accept-loop and RESP-setup halves only.
2. **Does `codex-usage-limit-capacity` outrank the medium slices on value?** It is the largest user-visible defect in the range (a per-turn 429 cooling a credential as quota-exhausted) and the largest commit; the recommendation defers it purely on size.
3. **`codex-client-model-catalog` merge direction** — confirm the local blobs stay authoritative (merge the restriction fields in) rather than syncing to upstream's catalog, since the local `models.json` carries entries upstream's does not.
