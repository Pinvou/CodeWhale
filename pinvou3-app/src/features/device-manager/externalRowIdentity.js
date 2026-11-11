// Effective armed/busy/list identity of an external-model row in the device
// manager. Pure function on the bridge row payload so tests can direct-hit the
// contract without mounting the view (same pattern as pinvou-model-link.js).
import { canonicalOllamaModelName } from '../model-store/pinvou-model-link.js';
//
// kind + effective model identity (round-21 review P2, external-review P2).
// The old kind+name key collided on two real routes: two registry rows sharing
// a name across ports (two inference services both named "qwen3") and an alias
// registry row {name:"foo:latest", model:"bar"} next to a daemon row named
// "foo:latest" — one arm click used to open two armed panels sharing one
// deleteWeights checkbox.
//
// Endpoint participation (round-23 review P2, external-review): for non-ollama
// kinds the endpoint is part of the identity — the design doc defines same
// model on different endpoints as independent services, so two such rows must
// not share the armed/busy key (one arm used to open both confirmation panels;
// the backend used to reject the second registration instead — both wrong).
// The pair is JSON-stringified so a model name containing '@' or a URL can't
// forge another row's key. For ollama the endpoint stays OUT of the identity:
// listing and removal go through the single configured daemon, and the flip
// pair (unregistered daemon row for model M <-> its registry row) must share
// the identity so armed/busy survives the quiet re-register id swap (round-3
// review P2) even when the daemon URL is a custom port the fallback const
// doesn't know.
//
// The effective identity (`model || name`, ollama folded to `:latest`
// lowercase) mirrors the backend's registry_identity: the backend register
// rejects same-(kind, identity, endpoint) records and read_registry warns on
// handwritten duplicates, which keeps the key unique.
//
// The ollama tag test delegates to canonicalOllamaModelName (round-36 review
// P3): the ruler is "last `/` segment decides the tag" — the whole-string
// `includes(':')` this module used before counted a host-port colon as a tag,
// so registry-form names (`192.168.1.5:5000/ns/qwen3`) got a different key on
// each side of the quiet re-register id swap even though the backend folds
// both to `...:latest`. Finer-than-backend keys never reopened the two-panel
// bug, but they silently disarmed the flip pair for that shape; sharing the
// one ruler the link matcher already uses removes the drift.
export function externalRowIdentity(row) {
  const kind = row?.kind || '';
  const effective = String(row?.model || row?.name || '');
  return kind === 'ollama'
    ? `${kind}:${canonicalOllamaModelName(effective)}`
    : `${kind}:${JSON.stringify([effective, row?.base_url || ''])}`;
}
