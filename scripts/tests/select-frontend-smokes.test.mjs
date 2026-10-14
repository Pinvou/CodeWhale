import assert from "node:assert/strict";
import test from "node:test";

import {
  FULL_FRONTEND_SMOKES,
  selectFrontendSmokes,
} from "../select-frontend-smokes.mjs";

const labels = (items) => items.map(({ kind, target }) => `${kind}:${target}`);
const fullLabels = labels(FULL_FRONTEND_SMOKES);

const fullSuiteCases = [
  [
    "shared frontend paths fail closed to the full browser suite",
    ["pinvou3-app/src/shared/i18n.js"],
  ],
  [
    "test infrastructure changes fail closed to the full browser suite",
    ["pinvou3-app/tests/ui_test_server.js"],
  ],
  [
    "frontend smoke runner changes fail closed to the full browser suite",
    ["scripts/run-frontend-smokes.mjs"],
  ],
  [
    "shared smoke manifest changes fail closed to the full browser suite",
    ["scripts/select-frontend-smokes.mjs"],
  ],
  [
    "Linux knowledge host package resources run the full suite",
    [
      "pinvou3-app/src-tauri/resources/platforms/linux/knowledge-host/pinvou-knowledge-host-helper",
    ],
  ],
  [
    "unknown frontend features fail closed to the full browser suite",
    ["pinvou3-app/src/features/search/SearchView.jsx"],
  ],
  [
    "an empty or unrelated diff fails closed",
    ["docs/ci.md"],
  ],
];

for (const [name, paths] of fullSuiteCases) {
  test(name, () => {
    assert.deepEqual(labels(selectFrontendSmokes(paths)), fullLabels);
  });
}

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
      "npm:test:composer-tools-smoke",
      "npm:test:composer-pending-enable-smoke",
      "node:tests/pet_selector_ui_smoke.js",
    ],
  );
});

test("account platform changes route all six concrete F05 browser smokes", () => {
  const selected = labels(selectFrontendSmokes(["pinvou3-app/src/features/account-platform/PersonalUsageWorkspace.jsx"]));
  for (const feature of ["model-selection", "model-lifecycle", "native-code", "billing-notice", "usage", "usage-governance"]) {
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

test("organization and member unit browsers run for feature and shared changes", () => {
  for (const target of ["test:account-platform-organization-ui", "test:account-platform-member-units-ui"]) {
    assert.ok(fullLabels.includes(`npm:${target}`));
    assert.ok(labels(selectFrontendSmokes(["pinvou3-app/src/features/account-platform/ModelPolicyWorkspace.jsx"])).includes(`npm:${target}`));
    assert.ok(labels(selectFrontendSmokes(["pinvou3-app/src/shared/i18n.js"])).includes(`npm:${target}`));
  }
});

test("registration bonus compiled browsers are kept in full and feature suites", () => {
  const target = "npm:test:account-platform-registration-bonus-ui";
  assert.ok(fullLabels.includes(target));
  assert.ok(labels(selectFrontendSmokes(["pinvou3-app/src/features/account-platform/AdminOperationsWorkspace.jsx"])).includes(target));
});

test("closure production and recovery browser command is selected in full and feature suites", () => {
  const target = "npm:test:account-platform-closure-ui";
  assert.ok(fullLabels.includes(target));
  assert.ok(labels(selectFrontendSmokes(["pinvou3-app/src/features/account-platform/AccountClosureWorkspace.jsx"])).includes(target));
  assert.ok(labels(selectFrontendSmokes(["pinvou3-app/src/shared/i18n.js"])).includes(target));
});
