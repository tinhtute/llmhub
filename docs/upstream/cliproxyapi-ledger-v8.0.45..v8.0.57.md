# Upstream ledger — cliproxyapi v8.0.45..v8.0.57

- generated: 2026-11-06T09:49:20Z
- upstream: https://github.com/router-for-me/CLIProxyAPI
- local baseline: `fb556afe8360`
- non-merge commits: 132

Disposition values: `already-present`, `adapt`, `reject`, `superseded-locally`, `defer`.
Every non-empty disposition needs a citation on both sides.

| Release | Commit | Date | Subject | Surfaces | Disposition | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| v8.0.46 | `8365de86b6b1` | 2026-10-25 | docs(readme): update Kimi model availability | `README.md`, `README_CN.md`, `README_JA.md` |  |  |
| v8.0.46 | `86d951cd5090` | 2026-10-25 | fix(openai/responses): drop response.metadata for non-Codex clients | `sdk/api` |  |  |
| v8.0.46 | `b4cbda591510` | 2026-10-26 | docs(readme): add GPT-6.1 Terra | `README.md`, `README_CN.md`, `README_JA.md` |  |  |
| v8.0.46 | `bc80710469dd` | 2026-10-26 | perf: index web-search support to remove registry write-lock convoy | `internal/registry` |  |  |
| v8.0.46 | `16c6b4bd8c03` | 2026-10-27 | perf(api): reuse response compression encoders with a smaller zstd window | `internal/api` |  |  |
| v8.0.46 | `c6425a6e6c7a` | 2026-10-27 | docs: add Ebb to "Who is with us?" | `README.md`, `README_CN.md`, `README_JA.md` |  |  |
| v8.0.46 | `3c948a580897` | 2026-10-27 | perf(executor): decode upstream zstd synchronously and sniff unlabeled bodies lazily | `internal/runtime` |  |  |
| v8.0.46 | `9717e325ba6a` | 2026-10-28 | fix(responses): reject late websocket interrupts after terminal events | `internal/runtime`, `sdk/api`, `sdk/cliproxy` |  |  |
| v8.0.46 | `ae65e26df2f8` | 2026-10-28 | feat(api): filter private metadata in OpenAI responses and improve SSE framing recovery | `sdk/api` |  |  |
| v8.0.46 | `a3e76891b1a1` | 2026-10-28 | refactor(api): reuse response compression encoders and add tests | `config.example.yaml`, `internal/api` |  |  |
| v8.0.46 | `58874f3bf392` | 2026-10-28 | feat(registry): index web-search support | `internal/registry` |  |  |
| v8.0.46 | `12f2cafe33b3` | 2026-10-28 | test(api): measure open-stream heap with empty encoder pools | `internal/api` |  |  |
| v8.0.46 | `65889d39f0bd` | 2026-10-28 | test: update tests for default template changes and environment compatibility | `internal/api`, `internal/client`, `internal/discovery` +4 |  |  |
| v8.0.47 | `535110e109e4` | 2026-10-28 | docs(readme): add Nia Live Demo to "Who is with us?" | `README.md`, `README_CN.md`, `README_JA.md` |  |  |
| v8.0.47 | `0fe38224e529` | 2026-10-28 | feat(pluginhost): support raw request body byte-range patching | `internal/pluginhost`, `internal/runtime`, `sdk/api` +3 |  |  |
| v8.0.47 | `e324e4814c2f` | 2026-10-28 | perf(pluginhost): allow frontend auth providers to opt out of request body delivery | `internal/pluginhost`, `sdk/pluginabi`, `sdk/pluginapi` |  |  |
| v8.0.47 | `90c447d3a872` | 2026-10-29 | fix(responses): use tsc_ item id prefix for Gemini tool search calls | `internal/runtime`, `internal/translator` |  |  |
| v8.0.48 | `d5eb4ca1d518` | 2026-10-28 | perf(usage): optimize stream usage observation and large payload parsing | `internal/runtime`, `internal/translator` |  |  |
| v8.0.48 | `bd5ec5310aba` | 2026-10-28 | feat(usage): capture structured Gemini, Vertex, AI Studio, and Antigravity usage | `internal/redisqueue`, `internal/runtime`, `sdk/cliproxy` |  |  |
| v8.0.48 | `01551f5f6b26` | 2026-10-28 | feat(usage): capture structured OpenAI and Codex usage across responses, chat, and images | `internal/runtime` |  |  |
| v8.0.48 | `24840fddf843` | 2026-10-28 | feat(usage): capture structured xAI usage across chat, responses, and media | `internal/redisqueue`, `internal/runtime`, `sdk/cliproxy` |  |  |
| v8.0.48 | `89e472f3251e` | 2026-10-28 | feat(usage): capture structured Anthropic Messages upstream usage details | `internal/redisqueue`, `internal/runtime`, `sdk/cliproxy` |  |  |
| v8.0.48 | `43068c8642f3` | 2026-10-29 | fix(auth): write the cooldown state next to the auth file when auth-dir is relative | `sdk/cliproxy` |  |  |
| v8.0.48 | `4cb98e3ccdc3` | 2026-10-29 | fix(logging): prevent deferred request retention on Responses WebSockets | `internal/logging`, `internal/runtime`, `sdk/api` |  |  |
| v8.0.48 | `82deffb9ca0d` | 2026-10-29 | fix(usage): preserve output_item.done tool counts and multiline SSE usage | `internal/runtime` |  |  |
| v8.0.48 | `a6b4477b4f05` | 2026-10-29 | fix(home): restrict websocket access in home mode | `internal/api`, `sdk/cliproxy` |  |  |
| v8.0.48 | `97dd3a4a2f0c` | 2026-10-29 | fix(usage): preserve stream failure usage, multipart image params, and provisional zero search counters | `internal/runtime` |  |  |
| v8.0.48 | `498170a49cc9` | 2026-10-29 | fix(aistudio): add relay SSE frame reassembly helper | `internal/runtime` |  |  |
| v8.0.48 | `7df437c8229e` | 2026-10-29 | fix(aistudio): buffer fragmented relay SSE frames | `internal/runtime` |  |  |
| v8.0.48 | `02ee313962ba` | 2026-10-29 | fix(devin): classify JSON control character errors | `internal/runtime` |  |  |
| v8.0.48 | `fa42f390c120` | 2026-10-29 | test(openai): assert websocket disconnect error event | `sdk/api` |  |  |
| v8.0.48 | `dbb706215a3a` | 2026-10-30 | fix(codex): expand recognized CPA compaction capsules before forwarding to OpenAI | `internal/runtime` |  |  |
| v8.0.48 | `068fe153efa6` | 2026-10-30 | fix(antigravity): preserve Claude tool-result error semantics | `internal/translator` |  |  |
| v8.0.48 | `81de4b9c48e8` | 2026-10-30 | fix(gemini): preserve Claude tool-result error semantics (#6838) | `internal/translator`, `internal/util` |  |  |
| v8.0.48 | `bc81923ba63a` | 2026-10-30 | feat(pluginhost): allow model registrars to publish models for multiple providers | `internal/pluginhost`, `sdk/cliproxy`, `sdk/pluginabi` +1 |  |  |
| v8.0.48 | `12d97cfe196b` | 2026-10-30 | fix(translator): translate mid-conversation Claude tool_addition blocks | `internal/translator`, `internal/util` |  |  |
| v8.0.48 | `514d09f746ff` | 2026-10-30 | feat(codex): guide recovery when automatic credential renewal becomes impossible | `docs/management-api-v8.md`, `internal/api`, `internal/auth` +3 |  |  |
| v8.0.48 | `f013eb662aa2` | 2026-10-30 | fix(openai): keep incomplete streams without [DONE] out of credential cooldown | `internal/runtime` |  |  |
| v8.0.49 | `250d598175db` | 2026-10-31 | perf(antigravity): make selectGroupResponses O(N+M) and deduplicate systemInstruction sanitize | `internal/signature`, `internal/translator` |  |  |
| v8.0.49 | `234217d081f1` | 2026-10-31 | fix(antigravity): discard excess functionResponse parts per turn in fixCLIToolResponse (#7137) | `internal/translator` |  |  |
| v8.0.49 | `f42f58719d4e` | 2026-10-31 | fix(antigravity): strip empty part objects from Gemini systemInstruction.parts | `internal/translator` |  |  |
| v8.0.49 | `a8dbebabdf0a` | 2026-10-31 | feat(pluginhost): allow plugins to register multiple OAuth login entries | `internal/api`, `internal/pluginhost`, `sdk/cliproxy` +3 |  |  |
| v8.0.49 | `4c46f0aaa575` | 2026-10-31 | fix(gemini): strip empty part objects from systemInstruction (#7136) | `internal/signature`, `internal/translator` |  |  |
| v8.0.49 | `cc4b9949e76b` | 2026-10-31 | feat(claude): add opt-in visible Codex commentary with preserved replay | `README.md`, `config.example.yaml`, `examples/claude-code-codex` +4 |  |  |
| v8.0.50 | `6c231c23321b` | 2026-10-29 | fix(proxy): lazily allocate uTLS pools and close fenced transports | `internal/runtime`, `internal/watcher` |  |  |
| v8.0.50 | `638f5f55e0d9` | 2026-10-29 | fix(proxy): honour per-key pool opt-in and isolate pools per runtime | `config.example.yaml`, `internal/config`, `internal/runtime` +2 |  |  |
| v8.0.50 | `9f0c9ff420c6` | 2026-10-29 | docs(config): document per-api-key connection pools | `config.example.yaml` |  |  |
| v8.0.50 | `34a01b7602b3` | 2026-10-29 | feat(proxy): pool upstream transports per api key | `internal/config`, `internal/runtime`, `sdk/cliproxy` +1 |  |  |
| v8.0.50 | `bf834311e6d6` | 2026-10-29 | feat(config): add per-api-key connection-pool settings | `internal/config`, `internal/watcher`, `sdk/cliproxy` |  |  |
| v8.0.50 | `bb6e1aa8c0e8` | 2026-10-30 | fix(meta): include Muse namespace in tool-bearing requests | `internal/runtime` |  |  |
| v8.0.50 | `5d8fddd338df` | 2026-10-30 | chore(meta): match Muse Code 1.4.14 client headers | `internal/runtime` |  |  |
| v8.0.50 | `893958e40169` | 2026-10-31 | fix(proxy): decline pooling when http.DefaultTransport is not *http.Transport | `internal/runtime` |  |  |
| v8.0.50 | `7d2959a79b48` | 2026-10-31 | fix(home): keep the release flusher idle when nothing is pending | `internal/home` |  |  |
| v8.0.50 | `f7331b519773` | 2026-11-01 | fix(pluginhost): deframe same-format plugin executor streams and gate hooks | `internal/pluginhost` |  |  |
| v8.0.50 | `08437a6742a5` | 2026-11-01 | fix(codex): keep shortened gateway mcp tool names readable in claude translator | `internal/translator` |  |  |
| v8.0.50 | `a21814ec608a` | 2026-11-01 | docs(config): note connection-pool pruning boundaries on save | `internal/config` |  |  |
| v8.0.50 | `d65840fb5c86` | 2026-11-01 | fix(xai): append identity hash to credential filenames to prevent collisions | `internal/api`, `internal/auth`, `internal/store` +1 |  |  |
| v8.0.50 | `9037d5430db9` | 2026-11-01 | fix(openai): drop output-only content_token_count from replayed responses websocket items | `sdk/api` |  |  |
| v8.0.50 | `41c03b74511a` | 2026-11-01 | fix(pluginhost): normalize same-format plugin executor streams through the stream translator | `internal/pluginhost` |  |  |
| v8.0.50 | `a96e640d886a` | 2026-11-01 | fix(auth): keep sign-in-required Codex credentials quarantined across manual refresh | `internal/api`, `internal/runtime`, `sdk/cliproxy` |  |  |
| v8.0.50 | `ecbaa6c710bb` | 2026-11-01 | fix(devin): route compound fast thinking levels to upstream fast variants | `internal/runtime` |  |  |
| v8.0.50 | `fd02b720e6b3` | 2026-11-01 | fix(proxy): stamp keepalive on uTLS dialers, rebind stale compat executors, and close retired pools outside fe | `internal/pluginhost`, `internal/runtime`, `sdk/cliproxy` +2 |  |  |
| v8.0.50 | `b63322b9c35a` | 2026-11-01 | fix(registry): preserve explicit thinkingLevel on Antigravity Claude 5.5 models | `internal/registry`, `internal/runtime` |  |  |
| v8.0.50 | `d00267c3fe24` | 2026-11-01 | fix(auth): treat Anthropic credit balance 400 errors as payment required | `internal/clienterror`, `sdk/cliproxy` |  |  |
| v8.0.51 | `b147b8d21053` | 2026-10-30 | fix(meta): include Muse namespace in tool-bearing requests | `internal/runtime` |  |  |
| v8.0.51 | `d9a688e61557` | 2026-10-30 | chore(meta): match Muse Code 1.4.14 client headers | `internal/runtime` |  |  |
| v8.0.51 | `c69cdd933d19` | 2026-10-31 | fix(xai): preserve Lite resolution and catalog fallback | `config.example.yaml`, `internal/registry`, `sdk/api` |  |  |
| v8.0.51 | `f0ddc38f2921` | 2026-10-31 | fix(xai): support Grok Imagine Video 1.5 Lite | `config.example.yaml`, `sdk/api` |  |  |
| v8.0.51 | `ec06c3cb8bac` | 2026-11-01 | fix(translator): normalize numeric schema constraints when converting parameters (#7149) | `internal/translator`, `internal/util`, `test/gemini_ref_sibling_test.go` |  |  |
| v8.0.51 | `53ff2f9e7478` | 2026-11-01 | feat(pluginhost): expose per-entry name and logo for multi-entry OAuth plugins | `internal/api`, `internal/pluginhost`, `sdk/pluginabi` +2 |  |  |
| v8.0.51 | `ce39bc620f65` | 2026-11-01 | feat(pluginstore): support plugin locales normalization and management passthrough | `internal/api`, `internal/pluginstore`, `sdk/pluginstore` |  |  |
| v8.0.51 | `9513c2d5dc85` | 2026-11-01 | docs(auth): explain Codex refresh token key requirement | `sdk/cliproxy` |  |  |
| v8.0.51 | `7b4c64b00687` | 2026-11-01 | fix(auth): preserve disabled credential refresh backoff | `sdk/cliproxy` |  |  |
| v8.0.51 | `95f125ffd04c` | 2026-11-01 | fix(auth): skip automatic Codex refresh without renewal credentials | `sdk/cliproxy` |  |  |
| v8.0.51 | `3ac384983ea2` | 2026-11-02 | fix(gemini): preserve parallel responses tool output batches across developer notices | `internal/translator` |  |  |
| v8.0.51 | `00b7677102fa` | 2026-11-02 | fix(translator): preserve developer roles in responses to chat completions | `internal/runtime`, `internal/translator` |  |  |
| v8.0.52 | `7c627ffbdb4a` | 2026-10-03 | fix(auth/antigravity): request aicode OAuth scope | `internal/auth` |  |  |
| v8.0.52 | `a5795047bfb4` | 2026-11-01 | fix(api/middleware): finalize request logging on handler panic | `internal/api` |  |  |
| v8.0.52 | `8643ea56d520` | 2026-11-01 | fix(config): include connection-pool settings in credential deduplication keys | `internal/api`, `internal/config`, `internal/watcher` +1 |  |  |
| v8.0.52 | `3698b1597aba` | 2026-11-01 | fix(pluginhost): scope model execution streams to the owning plugin and instance | `internal/pluginhost` |  |  |
| v8.0.52 | `8a82bed5825e` | 2026-11-01 | fix(helps): release TransportCache mutex on builder panic and retire purged entries | `internal/runtime` |  |  |
| v8.0.52 | `d96b19416846` | 2026-11-02 | fix(pluginhost): reject model stream registration after concurrent teardown | `internal/pluginhost` |  |  |
| v8.0.52 | `c888ff72d462` | 2026-11-02 | fix(translator): normalize omitted and function roles in Gemini ingress translators | `internal/translator` |  |  |
| v8.0.52 | `6e6afab99a19` | 2026-11-03 | refactor: migrate gjson, sjson, and base64 to MoeForge/moego | `cmd/fetch_antigravity_models`, `cmd/fetch_codex_models`, `examples/plugin` +28 |  |  |
| v8.0.52 | `be2563ff74a4` | 2026-11-03 | docs: update Xcode sponsorship copy | `README.md`, `README_CN.md`, `README_JA.md` |  |  |
| v8.0.52 | `b1a3e50bdc33` | 2026-11-03 | docs: remove OpenLux sponsorship from READMEs | `README.md`, `README_CN.md`, `README_JA.md` |  |  |
| v8.0.53 | `01b24f1f95dc` | 2026-11-03 | fix(codex): preserve and forward max_output_tokens across request translators | `internal/runtime`, `internal/translator` |  |  |
| v8.0.53 | `cfa25b4a0a80` | 2026-11-03 | refactor(translator): build Devin thought step with a single ojson.Doc | `internal/translator` |  |  |
| v8.0.53 | `b3442f6544c5` | 2026-11-03 | fix(devin): keep sealed reasoning items sent back via the Responses API | `internal/translator` |  |  |
| v8.0.53 | `be8643051801` | 2026-11-03 | fix(translator/gemini): nest Claude tool_result images inside functionResponse.parts | `internal/translator` |  |  |
| v8.0.53 | `04208efa8b52` | 2026-11-03 | fix(pluginhost): preserve disabled auth state across plugin refreshes | `internal/pluginhost` |  |  |
| v8.0.53 | `6d4102f33f10` | 2026-11-03 | feat(config): support native-entrypoints configuration for Claude Code clients | `config.example.yaml`, `internal/config`, `internal/runtime` |  |  |
| v8.0.53 | `54778bd30de5` | 2026-11-03 | fix(agents): enforce usage of moego libraries for JSON and Base64 operations | `AGENTS.md` |  |  |
| v8.0.53 | `dde469d3c2cf` | 2026-11-03 | fix(pluginhost): isolate unix plugin dlopen crashes with a preflight subprocess probe | `examples/plugin`, `internal/pluginhost` |  |  |
| v8.0.53 | `9acf81d3a673` | 2026-11-03 | fix(claude): drop replayed web search citations when search results are missing | `internal/runtime`, `internal/translator` |  |  |
| v8.0.53 | `a1eeb60ec3f7` | 2026-11-03 | fix(codex): disable supports_image_detail_original for non-codex catalog routes | `internal/client` |  |  |
| v8.0.53 | `d38997717dd3` | 2026-11-03 | fix(management): honor targeted OAuth re-login across all built-in providers | `internal/api`, `sdk/cliproxy` |  |  |
| v8.0.53 | `24b0454811d2` | 2026-11-03 | fix(antigravity): map safety and filter finish reasons to refusal in claude response translator | `internal/translator` |  |  |
| v8.0.53 | `e25b7dafa0ea` | 2026-11-03 | fix(devin): sanitize embedded exec tool phrases and stop retrying request-caused 403 errors | `internal/clienterror`, `internal/runtime`, `internal/translator` +1 |  |  |
| v8.0.53 | `0130d6f38c91` | 2026-11-03 | fix(claude): keep Fable-only 429 rejections model-scoped when 5h headers are omitted | `internal/runtime` |  |  |
| v8.0.53 | `e582706557b2` | 2026-11-03 | fix(auth): prevent auto-refresh spin for disabled credentials with rejected refresh tokens | `sdk/cliproxy` |  |  |
| v8.0.54 | `6d7e6a3bb9da` | 2026-11-03 | feat(usage): extract cached image/audio and interactions modality tokens | `internal/redisqueue`, `internal/runtime`, `sdk/cliproxy` |  |  |
| v8.0.54 | `78a9cc9aaa8a` | 2026-11-04 | chore(deps): update github.com/router-for-me/moego to v0.1.2 | `go.mod`, `go.sum` |  |  |
| v8.0.54 | `0e02765ccfce` | 2026-11-04 | fix(auth): clone parsed model IDs and preserve nil unknown catalogs | `sdk/auth` |  |  |
| v8.0.54 | `f49bd5d91ac9` | 2026-11-04 | refactor(auth): route Antigravity model JSON operations through ojson | `sdk/auth` |  |  |
| v8.0.54 | `f3762c64de51` | 2026-11-04 | feat(auth): extract Antigravity model discovery to sdk/auth | `sdk/auth`, `sdk/cliproxy` |  |  |
| v8.0.54 | `f72f8bfd6a1c` | 2026-11-04 | feat(antigravity): bump fallback version to 2.29.0 and add changelist mapping | `internal/misc` |  |  |
| v8.0.54 | `6e1035996839` | 2026-11-04 | fix(pluginhost): abort hot reload on quiesce failure and coordinate usage replay | `internal/pluginhost` |  |  |
| v8.0.54 | `606e667a8ef1` | 2026-11-04 | fix(codex): sanitize invalid characters in Codex input item IDs | `internal/runtime` |  |  |
| v8.0.54 | `061ffe6fbc22` | 2026-11-04 | fix(devin): honor Claude mid-conversation effort directives when selecting model variants | `internal/runtime` |  |  |
| v8.0.54 | `fb30a9e8bb4a` | 2026-11-04 | fix(thinking): prefer Claude mid-conversation effort directives in usage extraction | `internal/thinking` |  |  |
| v8.0.55 | `e6e5708e677a` | 2026-11-04 | feat(claude): persist cloaked session date pins in persistent KV store | `config.example.yaml`, `internal/config`, `internal/runtime` |  |  |
| v8.0.55 | `d57270f465cc` | 2026-11-04 | fix(executor): support Codex remote compaction v2 on openai-compatible routes | `internal/runtime` |  |  |
| v8.0.55 | `ec14dc4703f0` | 2026-11-04 | feat(kvstore): add local bbolt key-value store and integration routing | `config.example.yaml`, `go.mod`, `go.sum` +6 |  |  |
| v8.0.56 | `508ac2bce096` | 2026-11-04 | fix(executor,handlers): require stream-level evidence for Gemini filter blocks and pre-log terminal chunk erro | `internal/clienterror`, `internal/runtime`, `sdk/api` |  |  |
| v8.0.56 | `03a8743c3cf9` | 2026-11-04 | fix(executor): classify non-stream Claude policy refusals as 400 request faults | `internal/runtime` |  |  |
| v8.0.56 | `69a032dea368` | 2026-11-04 | perf,fix(logging,websocket): avoid 200 OK body buffering and synchronize ws disconnect error logging | `internal/api`, `internal/clienterror`, `sdk/api` |  |  |
| v8.0.56 | `0a94b2aefb52` | 2026-11-04 | fix(logging,executor): preserve Claude Fast classified error status and cap deferred body buffering | `internal/api`, `internal/runtime` |  |  |
| v8.0.56 | `c3938b72bdaa` | 2026-11-04 | fix(logging,clienterror): classify mid-stream cyber blocks as 400 and persist error logs | `internal/api`, `internal/clienterror`, `internal/logging` +2 |  |  |
| v8.0.56 | `585f97deac9a` | 2026-11-05 | fix(auth): pass context and headers to availableAuthsForSelector in HasAlternativeTargetForModel | `sdk/cliproxy` |  |  |
| v8.0.56 | `b5e6027ce4d7` | 2026-11-05 | fix(openai): expose terminal 429 error frames on responses websocket | `sdk/api`, `sdk/cliproxy` |  |  |
| v8.0.56 | `dfe8b182c54d` | 2026-11-05 | fix(codex,auth): bypass SIWC for OpenAI image requests and route directly to Codex images endpoint | `internal/runtime`, `sdk/cliproxy` |  |  |
| v8.0.56 | `be69162478da` | 2026-11-05 | fix(executor,logging): preserve Antigravity usage evidence, sparse Claude refusal identity, and Home stream er | `internal/api`, `internal/clienterror`, `internal/logging` +2 |  |  |
| v8.0.56 | `6044c5d14270` | 2026-11-05 | fix(openai): translate thinking content-part arrays in Claude response translator | `internal/translator` |  |  |
| v8.0.56 | `8c9a78518e39` | 2026-11-05 | fix(openai): bridge client tool_search in Chat Completions Responses translator | `internal/translator` |  |  |
| v8.0.56 | `7a7d37ecb5c6` | 2026-11-05 | feat(codex): add support for gpt-image-2.6 models | `internal/api`, `internal/client`, `internal/registry` +3 |  |  |
| v8.0.56 | `f208c90824f2` | 2026-11-05 | fix(handlers): record TTFT for direct plugin executor routes | `sdk/api` |  |  |
| v8.0.57 | `c2f402b45643` | 2026-11-05 | fix(meta): send X-Tbh-Contributor-Mode header only for contributor models | `internal/runtime` |  |  |
| v8.0.57 | `1c5372116735` | 2026-11-05 | fix(auth): keep count_tokens 501 and unsupported capability responses availability-neutral | `internal/pluginhost`, `sdk/cliproxy`, `sdk/pluginabi` |  |  |
| v8.0.57 | `a3c84a37c3a7` | 2026-11-05 | feat(devin): update devin_models.json with swe-2-flash, nemotron-3-5-super, and gpt-6-1-terra | `internal/registry`, `internal/runtime` |  |  |
| v8.0.57 | `8fb60326f405` | 2026-11-05 | fix(codex): handle Responses-Lite hosted image_generation compatibility | `internal/registry`, `internal/runtime`, `sdk/api` |  |  |
| v8.0.57 | `f2e255f73eda` | 2026-11-05 | fix(usage): clear consumed queue slot in usage manager dispatcher | `sdk/cliproxy` |  |  |
