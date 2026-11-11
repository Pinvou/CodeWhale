"""Require actual Portal Redis roots and capacity scenarios; skips are failures."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

ROOTS = {
    'TestRealRedisApprovalWireSelectionIsImmutableAndSessionBound',
    'TestRealRedisApprovalWireEligibilityAndCorruptionFailClosed',
    'TestRealRedisLoginCallbackPreservesLineageAndLogoutFence',
    'TestRealRedisRefreshCancellationBoundaries',
    'TestRealRedisRefreshBrowserDisconnectPreservesRotatedCredential',
    'TestRealRedisLineageLogoutFencesAllGenerationsAndPendingFlow',
    'TestRealRedisHandoffReplacementIsAtomicAndTicketSingleUse',
    'TestRealRedisHandoffThroughProductionHTTPReplacesAndCannotReplay',
    'TestRealRedisSignedCallbackAfterLogoutCannotRestoreIdentity',
    'TestRealRedisFlowConsumptionAndSessionTTL',
    'TestRealRedisRefreshLeaseHandoffFencesStaleOwnerAndLogout',
    'TestRealRedisLogoutDuringHTTPRefreshCannotResurrectSession',
    'TestRealRedisConcurrentSessionHTTPRequestsRefreshExactlyOnce',
    'TestRealRedisTransportInterruptionReturns503WithoutLosingSession',
    'TestRealRedisPortalSharedAdmissionAndCapacity',
}
CAPACITY_CASES = {
    'AllFourOperationsShareAtomicBurstAcrossClients', 'PolicyMismatchAndCorruptionNeverGrantFreshBurst',
    'FlowCapacityRetainsOriginalAndRecoversAfterSingleUse', 'ConcurrentCreatorsCannotOversubscribe',
    'HandoffReservationRetainsUnknownWriteAndExpires', 'LegacyInventoryPreservesNativeTTLAndAllowsConsumers',
    'PersistentLegacyObjectFailsInventoryWithoutDeletingEvidence', 'ACLFailurePrecedesOneUseAndCapacityMutations',
    'PhysicalOOMPreservesAcceptedObjectsAndDeletionRecovery',
    'PublicRoutersRejectBeforeConsumingOriginalObjects', 'UnknownSaveAndConsumptionRepliesAreNotAutomaticallyRetried',
    'BackgroundInventoryProgressesAndStops',
    'PersonalProductionACLServesAllFourAdmissionAndCapacityOperations',
    'WrongTypeLegacyObjectsFailInventoryWithoutMutation',
}


def verify(events):
    passed, ran, failures = set(), set(), []
    packages = 0
    for event in events:
        name, action = event.get('Test'), event.get('Action')
        if action in ('skip', 'fail'):
            failures.append(name or event.get('Package', 'unknown'))
        if name and action == 'run':
            ran.add(name)
        if name and action == 'pass':
            passed.add(name)
        if not name and action == 'pass':
            packages += 1
    expected = ROOTS | {'TestRealRedisPortalSharedAdmissionAndCapacity/' + name for name in CAPACITY_CASES}
    actual_roots = {name for name in ran if '/' not in name}
    if actual_roots != ROOTS:
        raise ValueError('unregistered or missing Portal Redis roots')
    missing = expected - (passed & ran)
    if missing or failures or packages != 1:
        raise ValueError('Portal Redis execution incomplete: missing=' + ','.join(sorted(missing)) + '; failures=' + ','.join(failures))
    return {'roots': len(ROOTS), 'capacity_children': len(CAPACITY_CASES), 'observed_children': len([x for x in passed if '/' in x]), 'skips': 0, 'failures': 0}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--log', type=Path, required=True)
    args = parser.parse_args()
    if not os.environ.get('PINVOU_PORTAL_TEST_REDIS_URL'):
        raise RuntimeError('owned Redis fixture is mandatory')
    command = [os.environ.get('PINVOU_GO_BINARY', 'go'), '-C', 'account-platform', 'test', '-race', '-tags=nomsgpack', '-mod=readonly', '-count=1', '-json', '-timeout=8m', '-run', '^TestRealRedis', './internal/portal']
    with args.log.open('w', encoding='utf-8') as output:
        result = subprocess.run(command, stdout=output, stderr=subprocess.STDOUT, timeout=600)
    events = []
    for line in args.log.read_text(encoding='utf-8').splitlines():
        try:
            events.append(json.loads(line))
        except ValueError:
            print(line)
    if result.returncode:
        print(args.log.read_text(encoding='utf-8')[-16000:])
        return result.returncode
    print(json.dumps(verify(events), sort_keys=True))
    return 0


if __name__ == '__main__':
    sys.exit(main())
