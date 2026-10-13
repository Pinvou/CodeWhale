package clientcompat

import (
	"fmt"
	"sort"
	"strings"
)

type semanticVersion struct {
	core       [3]string
	prerelease []string
}

// ValidVersion validates the complete, unmodified SemVer 2.0.0 string.
// Numeric identifiers have no machine-integer or additional length limit.
func ValidVersion(value string) bool {
	_, err := parseVersion(value)
	return err == nil
}

func parseVersion(value string) (semanticVersion, error) {
	invalid := func() (semanticVersion, error) {
		return semanticVersion{}, fmt.Errorf("invalid semantic version %q", value)
	}
	main, build, hasBuild := strings.Cut(value, "+")
	if hasBuild && !validIdentifiers(build, false) {
		return invalid()
	}
	core, prerelease, hasPrerelease := strings.Cut(main, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return invalid()
	}
	var version semanticVersion
	for i, part := range parts {
		if !numericIdentifier(part) || (len(part) > 1 && part[0] == '0') {
			return invalid()
		}
		version.core[i] = part
	}
	if hasPrerelease {
		if !validIdentifiers(prerelease, true) {
			return invalid()
		}
		version.prerelease = strings.Split(prerelease, ".")
	}
	return version, nil
}

func numericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func validIdentifiers(value string, prerelease bool) bool {
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		for i := 0; i < len(identifier); i++ {
			c := identifier[i]
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-') {
				return false
			}
		}
		if prerelease && numericIdentifier(identifier) && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func compareNumeric(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
}

func compareParsed(left, right semanticVersion) int {
	for i := range left.core {
		if comparison := compareNumeric(left.core[i], right.core[i]); comparison != 0 {
			return comparison
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) != 0 {
		return 1
	}
	if len(right.prerelease) == 0 && len(left.prerelease) != 0 {
		return -1
	}
	for i := 0; i < len(left.prerelease) && i < len(right.prerelease); i++ {
		a, b := left.prerelease[i], right.prerelease[i]
		aNumeric, bNumeric := numericIdentifier(a), numericIdentifier(b)
		var comparison int
		switch {
		case aNumeric && bNumeric:
			comparison = compareNumeric(a, b)
		case aNumeric:
			comparison = -1
		case bNumeric:
			comparison = 1
		default:
			comparison = strings.Compare(a, b)
		}
		if comparison != 0 {
			return comparison
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1
	}
	return 0
}

// CompareVersions returns -1, 0 or 1 by SemVer precedence. Build metadata is
// validated but ignored for ordering; neither input is normalized or coerced.
func CompareVersions(left, right string) (int, error) {
	a, err := parseVersion(left)
	if err != nil {
		return 0, err
	}
	b, err := parseVersion(right)
	if err != nil {
		return 0, err
	}
	return compareParsed(a, b), nil
}

// SortVersions sorts descending by SemVer precedence, keeping the original
// order of equal-precedence build variants. Invalid input leaves the slice
// untouched and returns an error before sorting any element.
func SortVersions(versions []string) error {
	type entry struct {
		raw    string
		parsed semanticVersion
	}
	entries := make([]entry, len(versions))
	for i, raw := range versions {
		parsed, err := parseVersion(raw)
		if err != nil {
			return err
		}
		entries[i] = entry{raw: raw, parsed: parsed}
	}
	sort.SliceStable(entries, func(i, j int) bool { return compareParsed(entries[i].parsed, entries[j].parsed) > 0 })
	for i, entry := range entries {
		versions[i] = entry.raw
	}
	return nil
}
