package com.pinvou.identity;

import org.junit.jupiter.api.Test;

import java.util.UUID;

import static org.junit.jupiter.api.Assertions.assertEquals;

final class PinvouIdentitySecurityResourceTest {
    private static UUID uuidV7(String suffix) {
        return UUID.fromString("01997a00-7000-7000-8000-" + suffix);
    }

    @Test
    void responseLossProbeObservesTheAtomicCredentialAndMarkerCommit() {
        UUID operation = uuidV7("000000000001");
        assertEquals(PinvouIdentitySecurityResource.FenceDecision.WRITE,
                PinvouIdentitySecurityResource.evaluateFence(0, null, operation, 7, true));
        assertEquals(PinvouIdentitySecurityResource.FenceDecision.APPLIED,
                PinvouIdentitySecurityResource.evaluateFence(7, operation.toString(), operation, 7, false));
    }

    @Test
    void secondProviderInstanceRejectsALateOlderOperation() {
        UUID newer = uuidV7("000000000002");
        UUID older = uuidV7("000000000001");
        // Instance A commits fence 8. A different provider instance receives
        // the delayed fence 7 request and must reject it from persisted state.
        assertEquals(PinvouIdentitySecurityResource.FenceDecision.CONFLICT,
                PinvouIdentitySecurityResource.evaluateFence(8, newer.toString(), older, 7, true));
    }

    @Test
    void equalFenceFromADifferentOperationIsRejected() {
        UUID winner = uuidV7("000000000002");
        UUID loser = uuidV7("000000000001");
        assertEquals(PinvouIdentitySecurityResource.FenceDecision.CONFLICT,
                PinvouIdentitySecurityResource.evaluateFence(9, winner.toString(), loser, 9, true));
        assertEquals(PinvouIdentitySecurityResource.FenceDecision.CONFLICT,
                PinvouIdentitySecurityResource.evaluateFence(9, null, loser, 9, true));
    }
}
