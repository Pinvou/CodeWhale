import assert from 'node:assert/strict';
import test from 'node:test';
import { retryControlledChatOpen, retryControlledSessionOpen } from '../src/features/remote-control/controlled-open-recovery.mjs';

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
