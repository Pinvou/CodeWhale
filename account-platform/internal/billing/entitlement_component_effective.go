package billing

// effectiveComponentOriginal is the current BASE/BONUS attribution used by
// provider reversals. Reclassification changes attribution, never the paid
// transaction amount or the total wallet liability.
func effectiveComponentOriginal(original, reclassifiedOut int64) (int64, error) {
	if original < 0 || reclassifiedOut < 0 || reclassifiedOut > original {
		return 0, ErrConflict
	}
	return original - reclassifiedOut, nil
}
