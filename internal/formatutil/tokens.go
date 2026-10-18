// Package formatutil provides dependency-light shared display formatting.
package formatutil

import "strconv"

// FormatTokens formats a signed token count using decimal units k, M, B, T, P,
// and E. Counts below 1000 in magnitude remain exact; compact counts are rounded
// to at most two decimal places with trailing zeros omitted. Rounding ties go
// away from zero, and rounding to 1000 promotes the count to the next unit.
func FormatTokens(n int64) string {
	if n > -1000 && n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	sign := ""
	magnitude := uint64(n)
	if n < 0 {
		sign = "-"
		// Avoid negating MinInt64 in the signed domain.
		magnitude = uint64(-(n + 1)) + 1
	}

	const units = "kMBTPE"
	divisor := uint64(1000)
	unit := 0
	for unit < len(units)-1 && magnitude/divisor >= 1000 {
		divisor *= 1000
		unit++
	}

	// Each step is one hundredth of the selected unit. Integer arithmetic
	// preserves rounding boundaries without overflowing even at MinInt64.
	step := divisor / 100
	hundredths := magnitude / step
	if magnitude%step >= step/2 {
		hundredths++
	}
	if hundredths >= 100000 && unit < len(units)-1 {
		hundredths = 100
		unit++
	}

	result := sign + strconv.FormatUint(hundredths/100, 10)
	if fraction := hundredths % 100; fraction != 0 {
		result += "." + strconv.FormatUint(fraction/10, 10)
		if fraction%10 != 0 {
			result += strconv.FormatUint(fraction%10, 10)
		}
	}
	return result + string(units[unit])
}
