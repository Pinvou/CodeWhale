import assert from "node:assert/strict";
import test from "node:test";

import {
  FULL_FRONTEND_SMOKES,
  selectFrontendSmokes,
} from "../select-frontend-smokes.mjs";

const labels = (items) => items.map(({ kind, target }) => `${kind}:${target}`);
const fullLabels = labels(FULL_FRONTEND_SMOKES);

test("shared frontend paths fail closed to the full browser suite", () => {
  assert.deepEqual(
    labels(selectFrontendSmokes(["pinvou3-app/src/shared/i18n.js"])),
    fullLabels,
  );
});

test("test infrastructure changes fail closed to the full browser suite", () => {
  assert.deepEqual(
    labels(selectFrontendSmokes(["pinvou3-app/tests/ui_test_server.js"])),
    fullLabels,
  );
});

test("frontend smoke runner changes fail closed to the full browser suite", () => {
  assert.deepEqual(
    labels(selectFrontendSmokes(["scripts/run-frontend-smokes.mjs"])),
    fullLabels,
  );
});

test("shared smoke manifest changes fail closed to the full browser suite", () => {
  assert.deepEqual(
    labels(selectFrontendSmokes(["scripts/select-frontend-smokes.mjs"])),
    fullLabels,
  );
});

test("feature-local changes select core and feature smokes", () => {
  assert.deepEqual(
    labels(
      selectFrontendSmokes([
        "README.md",
        "pinvou3-app/src/features/settings/SettingsView.jsx",
        "pinvou3-app/src/features/pet/PetWindow.jsx",
      ]),
    ),
    [
      "npm:test:ui-smoke",
      "npm:test:settings-ui",
      "node:tests/pet_selector_ui_smoke.js",
    ],
  );
});

test("account platform changes route all five concrete F05 browser smokes", () => {
  const selected = labels(selectFrontendSmokes(["pinvou3-app/src/features/account-platform/PersonalUsageWorkspace.jsx"]));
  for (const feature of ["model-selection", "native-code", "billing-notice", "usage", "usage-governance"]) {
    assert.ok(selected.includes(`npm:test:account-platform-${feature}-ui`), `${feature} browser smoke must run for account changes`);
    assert.ok(labels(selectFrontendSmokes(["pinvou3-app/src/shared/i18n.js"])).includes(`npm:test:account-platform-${feature}-ui`), `${feature} browser smoke must run for shared changes`);
  }
});

test("knowledge changes select the desktop knowledge smoke", () => {
  assert.deepEqual(
    labels(
      selectFrontendSmokes([
        "pinvou3-app/src/features/knowledge/KnowledgeBaseView.jsx",
      ]),
    ),
    [
      "npm:test:ui-smoke",
      "npm:test:kb-smoke",
    ],
  );
});

test("Linux knowledge host package resources run the full suite", () => {
  assert.deepEqual(
    labels(selectFrontendSmokes([
      "pinvou3-app/src-tauri/resources/platforms/linux/knowledge-host/pinvou-knowledge-host-helper",
    ])),
    fullLabels,
  );
});

test("relay changes select the web UI smoke", () => {
  assert.deepEqual(
    labels(selectFrontendSmokes(["remote-control-relay/server.js"])),
    ["npm:test:ui-smoke", "npm:test:webui"],
  );
});

test("team relay changes select its action smoke", () => {
  assert.deepEqual(
    labels(selectFrontendSmokes(["pinvou3-app/src/features/team-relay/TeamRelayView.jsx"])),
    ["npm:test:ui-smoke", "npm:test:team-relay-ui"],
  );
});

test("unknown frontend features fail closed to the full browser suite", () => {
  assert.deepEqual(
    labels(selectFrontendSmokes(["pinvou3-app/src/features/search/SearchView.jsx"])),
    fullLabels,
  );
});

test("an empty or unrelated diff fails closed", () => {
  assert.deepEqual(labels(selectFrontendSmokes(["docs/ci.md"])), fullLabels);
});
