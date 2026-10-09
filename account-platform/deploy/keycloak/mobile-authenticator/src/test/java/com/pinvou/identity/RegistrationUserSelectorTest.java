package com.pinvou.identity;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;

import java.util.List;
import org.junit.jupiter.api.Test;

final class RegistrationUserSelectorTest {
    private static final String OPERATION_ID = "0199a307-45ac-7e12-9f03-1bb29eb08fd6";

    private record Candidate(String name, String operationID) {}

    @Test
    void rejectsZeroExactMatchesIncludingFuzzyCandidates() {
        List<Candidate> candidates = List.of(
            new Candidate("prefix", OPERATION_ID + "-other"),
            new Candidate("suffix", "other-" + OPERATION_ID),
            new Candidate("case", OPERATION_ID.toUpperCase())
        );
        assertNull(RegistrationUserSelector.uniqueExactMatch(candidates, OPERATION_ID, Candidate::operationID));
    }

    @Test
    void selectsOneExactMatchWhileIgnoringNonMatchingCandidates() {
        Candidate expected = new Candidate("exact", OPERATION_ID);
        List<Candidate> candidates = List.of(
            new Candidate("fuzzy", OPERATION_ID + "0"),
            expected,
            new Candidate("unrelated", "0199a307-45ac-7e12-9f03-1bb29eb08fd7")
        );
        assertEquals(expected, RegistrationUserSelector.uniqueExactMatch(candidates, OPERATION_ID, Candidate::operationID));
    }

    @Test
    void rejectsTwoExactMatchesEvenWhenOtherCandidatesDoNotMatch() {
        List<Candidate> candidates = List.of(
            new Candidate("first", OPERATION_ID),
            new Candidate("fuzzy", OPERATION_ID + "-other"),
            new Candidate("second", OPERATION_ID)
        );
        assertNull(RegistrationUserSelector.uniqueExactMatch(candidates, OPERATION_ID, Candidate::operationID));
    }
}
