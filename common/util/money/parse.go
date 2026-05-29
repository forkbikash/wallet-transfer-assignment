package money

import (
	"fmt"
	"math"
	"strings"
)

// The ByteByteGo chapter sends the transfer `amount` as a STRING, not a float,
// to avoid losing precision. We parse that string into int64 minor units at the
// system boundary and never touch floating point.

// currencyExponent maps an ISO-4217 code to the number of minor-unit digits.
// Most currencies use 2 (cents/paisa); a few (e.g. JPY, KRW) use 0. Unknown
// codes default to 2 via Exponent.
var currencyExponent = map[string]int{
	"USD": 2, "EUR": 2, "GBP": 2, "INR": 2, "AUD": 2, "CAD": 2, "SGD": 2,
	"JPY": 0, "KRW": 0,
}

// Exponent returns the minor-unit exponent for an ISO-4217 code, defaulting to
// 2 for codes not in the table.
func Exponent(currency string) int {
	if e, ok := currencyExponent[strings.ToUpper(currency)]; ok {
		return e
	}
	return 2
}

// ParseMinor parses a non-negative decimal string ("100", "100.50") into Money
// (int64 minor units) for the given currency. It rejects negative values,
// malformed input, and more fractional digits than the currency allows. There
// is no floating point on this path.
func ParseMinor(s, currency string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("money: empty amount")
	}
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("money: amount must be a non-negative number: %q", s)
	}

	exp := Exponent(currency)

	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > exp {
		return 0, fmt.Errorf("money: %q has more than %d fractional digits for %s", s, exp, currency)
	}

	var whole int64
	for _, r := range intPart {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("money: invalid amount %q", s)
		}
		// Overflow-safe accumulate: whole*10 + digit.
		if whole > (math.MaxInt64-9)/10 {
			return 0, fmt.Errorf("money: amount %q overflows int64", s)
		}
		whole = whole*10 + int64(r-'0')
	}

	var frac int64
	for _, r := range fracPart {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("money: invalid amount %q", s)
		}
		frac = frac*10 + int64(r-'0')
	}
	// Right-pad the fractional part to `exp` digits ("5" with exp=2 -> 50).
	for range exp - len(fracPart) {
		frac *= 10
	}

	scale := int64(1)
	for range exp {
		scale *= 10
	}
	if whole > (math.MaxInt64-frac)/scale {
		return 0, fmt.Errorf("money: amount %q overflows int64", s)
	}
	return Money(whole*scale + frac), nil
}

// StringFor formats Money back into a decimal string with the currency's
// minor-unit precision (e.g. 10050 USD -> "100.50", 100 JPY -> "100").
func (m Money) StringFor(currency string) string {
	exp := Exponent(currency)
	n := int64(m)
	neg := n < 0
	if neg {
		n = -n
	}
	if exp == 0 {
		if neg {
			return "-" + fmt.Sprintf("%d", n)
		}
		return fmt.Sprintf("%d", n)
	}
	scale := int64(1)
	for range exp {
		scale *= 10
	}
	whole := n / scale
	frac := n % scale
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%0*d", sign, whole, exp, frac)
}
