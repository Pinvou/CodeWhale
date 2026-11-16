# Host model HTTP policies

An embedding host can install an in-memory `ModelHttpPolicy` on
`Config.model_http_policy`. The primary model and its HTTP/1 fallback each
build a client through this policy. Vision inherits the same capability;
`Config.vision_model_http_policy` supplies an independent vision override.
These fields are not accepted from TOML/JSON or persisted to settings.

`VisionModelConfig` retains its original three fields (`model`, `api_key`,
`base_url`), so existing complete host struct literals continue to compile.
`Config::vision_model_config()` still resolves that configuration and its
inherited API key. `Config::vision_http_policy()` resolves the separate host
capability. Runtime API disk reloads and provider switches preserve both host
capabilities, including disabling and re-enabling vision. CLI/TUI config
reloads require the embedding host to reinstall its capabilities. This is
capability retention, not live migration of an already loaded Engine's vision
route: existing engines retain their initial vision tool selection. New engines, and tools
constructed from a fresh Config snapshot, consume the updated route.

Built-in Engine, direct workflow and child registries carry the capability
to the vision builder. A host that registers vision directly can use
`ToolRegistryBuilder::with_vision_http_policy` before `with_vision_tools`.
For standalone vision without a primary model client, the public registry
supports `with_vision_http_policy(policy).with_vision_tools(config, None)`.
The internal tool also retains its existing route-client constructor.

Model builders apply `SSL_CERT_FILE` extra roots (PEM bundle or DER) with
reqwest's standard platform-verifier backend. Android retains its existing
preconfigured Mozilla-root backend, which ignores these additional root
settings; this change does not add Android custom-CA support. The shared
`CODEWHALE_FORCE_HTTP1` (legacy `DEEPSEEK_FORCE_HTTP1`) default remains. A malformed
trust file does not disable certificate verification. Library-level vision
metadata inheritance requires an exact supported chat-completions route match
with logical-call idempotency enabled. Those matches retain the primary route's
headers, user agent and connection defaults; unrelated routes do not borrow its
authentication or context headers.
The trusted host policy runs after these defaults and can explicitly override
them, including TLS certificate validation. This is a capability for trusted
in-process host code, not an isolation boundary for untrusted policy plugins.
Only the embedding host can install an implementation; deserializing settings
cannot install one. Vision request authentication is still applied at request time.

Speech synthesis uses the model client and is covered. Voice transcription,
prompt suggestions, credential probes, MCP servers and separate Fleet worker
processes are outside this in-process hook. Constructing a primary client
invokes its hook twice (normal and HTTP/1 fallback); each vision client invokes
its selected hook once. Policies should configure builders without assuming
one callback per logical request. Callback and host-modified builder failures
expose only a safe setup stage; a host needing private diagnostics should
record them inside its callback. They require host configuration correction,
not credential recovery or automatic provider fallback.
This capability changes transport only;
it does not add prompt/session data or alter the KV-cache prefix.
