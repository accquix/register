package normalize

// Current picks the current entry of a time-bound attribute (RPO "TimedValue" lists).
//
// Rule: an open entry (empty validTo) wins; among open entries the latest validFrom wins.
// If no entry is open (a terminated subject has every attribute closed), the entry with the
// latest validTo wins, then the latest validFrom. Dates are ISO "YYYY-MM-DD" strings, so they
// compare lexically. ok is false for an empty list.
func Current[T any](items []T, validFrom, validTo func(T) string) (best T, ok bool) {
	for _, it := range items {
		if !ok || better(validFrom(it), validTo(it), validFrom(best), validTo(best)) {
			best, ok = it, true
		}
	}
	return best, ok
}

func better(fromA, toA, fromB, toB string) bool {
	openA, openB := toA == "", toB == ""
	if openA != openB {
		return openA
	}
	if !openA && toA != toB {
		return toA > toB
	}
	return fromA > fromB
}
