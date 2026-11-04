import assert from 'node:assert/strict';
import test from 'node:test';
import { retryControlledChatOpen, retryControlledSessionOpen } from '../src/features/remote-control/controlled-open-recovery.mjs';

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

test('manual chat retry does not clear failure when ownership changes during navigation', async () => {
  let current = true;
  const gate = deferred();
  const opened = { current: null };
  let cleared = false;
  const owner = { generation: 'g1', intent: 1, navigationIsCurrent: () => current };
  const task = retryControlledChatOpen({
    failure: { generation: 'g1', originView: 'settings', owner },
    controlledDeviceAutoOpenInFlightRef: { current: null }, controlledRecoveryBusy: false,
    bsRef: { current: { controlledDevice: { status: 'connected', generation: 'g1' } } },
    currentViewRef: { current: 'settings' },
    bridge: { sessions: { isControlledNavigationOwnerCurrent: () => current } },
    setControlledRecoveryBusy() {}, navigateFromScheduledRun: () => gate.promise,
    controlledDeviceAutoOpenRef: opened, setControlledChatOpenFailure: () => { cleared = true; },
  });
  current = false;
  gate.resolve(true);
  await task;
  assert.equal(opened.current, null);
  assert.equal(cleared, false);
});

for (const stage of ['exit', 'switch']) {
  test(`manual session retry preserves failure when ownership changes during ${stage}`, async () => {
    let current = true;
    const gate = deferred();
    const entered = deferred();
    const opened = { current: null };
    let cleared = false;
    let activated = false;
    const owner = { generation: 'g1', intent: 1, navigationIsCurrent: () => current };
    const task = retryControlledSessionOpen({
      failure: { generation: 'g1', sessionId: 'session-b', owner },
      controlledSessionAutoOpenInFlightRef: { current: null }, controlledRecoveryBusy: false,
      bsRef: { current: { controlledDevice: { status: 'connected', generation: 'g1' }, controlledSession: { sessionId: 'session-b' }, scheduledRunContext: stage === 'exit' ? {} : null } },
      bridge: { available: true, sessions: {
        isControlledNavigationOwnerCurrent: () => current,
        exitControlledScheduledRunChat: () => { entered.resolve(); return gate.promise; },
        switchToControlledSession: () => { entered.resolve(); return gate.promise; },
      } },
      setControlledRecoveryBusy() {}, runBrowserUiTransition: fn => fn({ isCurrent: () => true }),
      setCodeModeOn() {}, setCurrentView() {}, setActiveChat() { activated = true; }, closeMobileSidebar() {},
      controlledSessionAutoOpenRef: opened, setControlledSessionOpenFailure: () => { cleared = true; },
    });
    await entered.promise;
    current = false;
    gate.resolve(true);
    await task;
    assert.equal(opened.current, null);
    assert.equal(cleared, false);
    assert.equal(activated, false);
  });
}

test('manual session retry does not mark an old owner open after the outer transition settles', async () => {
  let current = true;
  const gate = deferred();
  const entered = deferred();
  const opened = { current: null };
  let cleared = false;
  const owner = { generation: 'g1', intent: 1, navigationIsCurrent: () => current };
  const task = retryControlledSessionOpen({
    failure: { generation: 'g1', sessionId: 'session-b', owner },
    controlledSessionAutoOpenInFlightRef: { current: null }, controlledRecoveryBusy: false,
    bsRef: { current: { controlledDevice: { status: 'connected', generation: 'g1' }, controlledSession: { sessionId: 'session-b' } } },
    bridge: { available: true, sessions: {
      isControlledNavigationOwnerCurrent: () => current,
      switchToControlledSession: async () => true,
    } },
    setControlledRecoveryBusy() {},
    runBrowserUiTransition: async fn => { const result = await fn({ isCurrent: () => true }); entered.resolve(); await gate.promise; return result; },
    setCodeModeOn() {}, setCurrentView() {}, setActiveChat() {}, closeMobileSidebar() {},
    controlledSessionAutoOpenRef: opened, setControlledSessionOpenFailure: () => { cleared = true; },
  });
  await entered.promise;
  current = false;
  gate.resolve();
  await task;
  assert.equal(opened.current, null);
  assert.equal(cleared, false);
});

test('manual chat recovery is owner bound and does not start a task', async () => {
  const owner = { generation: 'g1', intent: 1 };
  const inFlight = { current: null };
  const opened = { current: null };
  let cleared = false;
  let navigations = 0;
  const deps = {
    failure: { generation: 'g1', originView: 'settings', owner },
    controlledDeviceAutoOpenInFlightRef: inFlight, controlledRecoveryBusy: false,
    bsRef: { current: { controlledDevice: { status: 'connected', generation: 'g1' } } },
    currentViewRef: { current: 'settings' },
    bridge: { sessions: { isControlledNavigationOwnerCurrent: value => value === owner } },
    setControlledRecoveryBusy() {},
    navigateFromScheduledRun: async (_view, _before, isCurrent) => { navigations += 1; return isCurrent(); },
    controlledDeviceAutoOpenRef: opened,
    setControlledChatOpenFailure: value => { cleared = value === null; },
  };
  await retryControlledChatOpen(deps);
  assert.equal(navigations, 1);
  assert.equal(opened.current, 'g1');
  assert.equal(cleared, true);
  assert.equal(inFlight.current, null);
  deps.bridge.sessions.isControlledNavigationOwnerCurrent = () => false;
  await retryControlledChatOpen(deps);
  assert.equal(navigations, 1);
});

test('manual session recovery ignores stale owners and only switches the exact session', async () => {
  const owner = { generation: 'g1', intent: 1 };
  const inFlight = { current: null };
  const opened = { current: null };
  let calls = 0;
  let cleared = false;
  const deps = {
    failure: { generation: 'g1', sessionId: 'session-b', owner },
    controlledSessionAutoOpenInFlightRef: inFlight, controlledRecoveryBusy: false,
    bsRef: { current: { controlledDevice: { status: 'connected', generation: 'g1' }, controlledSession: { sessionId: 'session-b' } } },
    bridge: { available: true, sessions: {
      isControlledNavigationOwnerCurrent: value => value === owner,
      switchToControlledSession: async (id, token) => { calls += 1; assert.equal(id, 'session-b'); assert.equal(token, owner); return true; },
    }, scheduled: {} },
    setControlledRecoveryBusy() {},
    runBrowserUiTransition: async fn => fn({ isCurrent: () => true }),
    setCodeModeOn() {}, setCurrentView() {}, setActiveChat() {}, closeMobileSidebar() {},
    controlledSessionAutoOpenRef: opened,
    setControlledSessionOpenFailure: value => { cleared = value === null; },
  };
  await retryControlledSessionOpen(deps);
  assert.equal(calls, 1);
  assert.equal(opened.current, 'g1:session-b');
  assert.equal(cleared, true);
  assert.equal(inFlight.current, null);
  deps.bsRef.current.controlledSession.sessionId = 'session-new';
  await retryControlledSessionOpen(deps);
  assert.equal(calls, 1);
});

test('manual recovery cannot switch after local navigation, including an ABA return', async () => {
  let epoch = 0;
  const captured = epoch;
  const owner = { generation: 'g1', intent: 1, navigationIsCurrent: () => epoch === captured };
  let switches = 0;
  const deps = {
    failure: { generation: 'g1', sessionId: 'session-b', owner },
    controlledSessionAutoOpenInFlightRef: { current: null }, controlledRecoveryBusy: false,
    bsRef: { current: { activeSessionId: 'local-a', controlledDevice: { status: 'connected', generation: 'g1' }, controlledSession: { sessionId: 'session-b' } } },
    bridge: { sessions: { isControlledNavigationOwnerCurrent: () => true, switchToControlledSession: async () => { switches += 1; return true; } } },
    setControlledRecoveryBusy() {}, runBrowserUiTransition: async fn => fn({ isCurrent: () => true }),
    setCodeModeOn() {}, setCurrentView() {}, setActiveChat() {}, closeMobileSidebar() {},
    controlledSessionAutoOpenRef: { current: null }, setControlledSessionOpenFailure() {},
  };
  epoch += 1;
  deps.bsRef.current.activeSessionId = 'local-b';
  epoch += 1;
  deps.bsRef.current.activeSessionId = 'local-a';
  await retryControlledSessionOpen(deps);
  assert.equal(switches, 0);
});
