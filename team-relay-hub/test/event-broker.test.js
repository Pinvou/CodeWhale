import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import test from "node:test";

import { EventBroker } from "../src/http.js";

function fakeResponse() {
  const response = new EventEmitter();
  response.endCount = 0;
  response.destroyCount = 0;
  response.write = () => true;
  response.end = () => {
    response.endCount += 1;
  };
  response.destroy = () => {
    response.destroyCount += 1;
  };
  return response;
}

function deferred() {
  let resolve;
  const promise = new Promise(resolvePromise => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

test("SSE authorization retries after a controlled concurrent team revocation", async () => {
  const broker = new EventBroker();
  const response = fakeResponse();
  const authorizationStarted = deferred();
  const resumeAuthorization = deferred();
  let authorizationAttempts = 0;
  const subscription = broker.authorizeAndSubscribe({
    authorize: async () => {
      authorizationAttempts += 1;
      if (authorizationAttempts === 1) {
        authorizationStarted.resolve();
        await resumeAuthorization.promise;
        return ["team-a"];
      }
      throw new Error("membership revoked");
    },
    principalId: "principal-a",
    response,
  });

  await authorizationStarted.promise;
  broker.revokeTeam("team-a");
  resumeAuthorization.resolve();

  await assert.rejects(subscription, /membership revoked/);
  assert.equal(authorizationAttempts, 2);
  assert.equal(broker.teams.has("team-a"), false);
});

test("member revocation between authorization and registration rejects the stale SSE subscription", () => {
  const broker = new EventBroker();
  const response = fakeResponse();
  const authorizationGeneration = broker.currentAuthorizationGeneration();

  broker.revoke("team-a", "principal-a");

  assert.equal(
    broker.subscribeAuthorized(["team-a"], "principal-a", response, authorizationGeneration),
    null,
  );
  assert.equal(broker.teams.has("team-a"), false);
});

test("team revocation between authorization and registration rejects the stale SSE subscription", () => {
  const broker = new EventBroker();
  const response = fakeResponse();
  const authorizationGeneration = broker.currentAuthorizationGeneration();

  broker.revokeTeam("team-a");

  assert.equal(
    broker.subscribeAuthorized(["team-a"], "principal-a", response, authorizationGeneration),
    null,
  );
  assert.equal(broker.teams.has("team-a"), false);
});

test("revocation after authorized registration closes the active SSE subscription", () => {
  const broker = new EventBroker();
  const response = fakeResponse();
  const unsubscribe = broker.subscribeAuthorized(
    ["team-a"],
    "principal-a",
    response,
    broker.currentAuthorizationGeneration(),
  );

  assert.equal(typeof unsubscribe, "function");
  broker.revoke("team-a", "principal-a");

  assert.equal(response.endCount, 1);
  assert.equal(response.destroyCount, 0);
  assert.equal(broker.teams.has("team-a"), false);
  unsubscribe();
});
