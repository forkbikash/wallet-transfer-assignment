package money

import "testing"

func TestParseMinor(t *testing.T) {
	ok := []struct {
		in       string
		currency string
		want     int64
	}{
		{"100", "USD", 10000},
		{"100.00", "USD", 10000},
		{"100.5", "USD", 10050},
		{"0.01", "USD", 1},
		{"0", "USD", 0},
		{"100", "JPY", 100}, // zero-exponent currency
		{"100", "jpy", 100}, // currency is case-insensitive
	}
	for _, c := range ok {
		got, err := ParseMinor(c.in, c.currency)
		if err != nil {
			t.Fatalf("ParseMinor(%q,%q) unexpected error: %v", c.in, c.currency, err)
		}
		if got.Minor() != c.want {
			t.Fatalf("ParseMinor(%q,%q) = %d, want %d", c.in, c.currency, got.Minor(), c.want)
		}
	}

	bad := []struct {
		in       string
		currency string
	}{
		{"100.001", "USD"},                // too many fractional digits
		{"100.1", "JPY"},                  // fractional digits not allowed for exponent 0
		{"-5", "USD"},                     // negative
		{"+5", "USD"},                     // signed
		{"abc", "USD"},                    // non-numeric
		{"1.2.3", "USD"},                  // malformed
		{"", "USD"},                       // empty
		{"9999999999999999999999", "USD"}, // overflow
	}
	for _, c := range bad {
		if _, err := ParseMinor(c.in, c.currency); err == nil {
			t.Fatalf("ParseMinor(%q,%q) expected error, got nil", c.in, c.currency)
		}
	}
}

func TestStringFor(t *testing.T) {
	cases := []struct {
		minor    int64
		currency string
		want     string
	}{
		{10000, "USD", "100.00"},
		{10050, "USD", "100.50"},
		{1, "USD", "0.01"},
		{0, "USD", "0.00"},
		{100, "JPY", "100"},
		{-2550, "USD", "-25.50"},
	}
	for _, c := range cases {
		got := FromMinor(c.minor).StringFor(c.currency)
		if got != c.want {
			t.Fatalf("StringFor(%d,%q) = %q, want %q", c.minor, c.currency, got, c.want)
		}
	}
}

// Round-trip: parsing a formatted amount yields the original minor units.
func TestParseStringForRoundTrip(t *testing.T) {
	for _, minor := range []int64{0, 1, 99, 10000, 123456} {
		s := FromMinor(minor).StringFor("USD")
		got, err := ParseMinor(s, "USD")
		if err != nil {
			t.Fatalf("round-trip parse %q: %v", s, err)
		}
		if got.Minor() != minor {
			t.Fatalf("round-trip %d -> %q -> %d", minor, s, got.Minor())
		}
	}
}
