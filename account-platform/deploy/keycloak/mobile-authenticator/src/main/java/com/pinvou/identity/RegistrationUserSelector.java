package com.pinvou.identity;

import java.util.List;
import java.util.function.Function;

/** Selects a registration identity only when exactly one candidate matches. */
final class RegistrationUserSelector {
    private RegistrationUserSelector() {}

    static <T> T uniqueExactMatch(List<T> candidates, String expectedOperationID, Function<T, String> operationID) {
        if (candidates == null || expectedOperationID == null || expectedOperationID.isBlank()) return null;
        T match = null;
        for (T candidate : candidates) {
            if (candidate == null || !expectedOperationID.equals(operationID.apply(candidate))) continue;
            if (match != null) return null;
            match = candidate;
        }
        return match;
    }
}
