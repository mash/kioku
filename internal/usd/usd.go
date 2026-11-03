// Package usd parses exact US dollar amounts for session budgets.
package usd

import (
	"errors"
	"strconv"
	"strings"
)

// MaxMicros is the largest amount that Parse accepts: $10,000.
const MaxMicros int64 = 10_000_000_000

// Parse converts a decimal dollar string such as "12.34" to integer
// millionths of a dollar. It rejects signs, exponents and more than six
// decimal places, so the result is exact.
func Parse(value string) (int64, error) {
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, errors.New("invalid USD amount")
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > 6 {
		return 0, errors.New("at most six decimal places are supported")
	}
	for _, c := range parts[0] + fraction {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid USD amount")
		}
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > 10000 {
		return 0, errors.New("amount exceeds $10,000")
	}
	fraction += strings.Repeat("0", 6-len(fraction))
	decimal, _ := strconv.ParseInt(fraction, 10, 64)
	valueMicros := whole*1_000_000 + decimal
	if valueMicros > MaxMicros {
		return 0, errors.New("amount exceeds $10,000")
	}
	return valueMicros, nil
}
