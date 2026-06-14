# Amount Calculation Best Practices (Beginner-Friendly)

How to handle money in code without losing or inventing cents. Written in plain language, with examples for the cases that aren't obvious.

---

## 1. Why floats break money

A "float" (`float64`, `double`) stores numbers in binary. Binary cannot represent most decimal fractions exactly — the same way decimal cannot write 1/3 exactly (0.3333… forever). So the computer stores a value *very close* to what you typed, but not equal to it.

**Example (run this in any language):**
```
0.1 + 0.2  ==>  0.30000000000000004
```
Each individual error is tiny. The problems:

- **Errors pile up.** Add 0.1 a million times — you won't get exactly 100000.0. In a wallet system doing millions of transactions, "tiny" becomes real money.
- **Comparisons fail.** `if balance == 0.30` is false even when it "should" be true. Reconciliation (checking that the books balance) needs *exact* equality.

**Rule: a float must never touch a money value — not in input parsing, not in math, not in the database, not in JSON.** One float anywhere in the chain corrupts the value.

---

## 2. The safe default: store money as whole integers

Don't store ₹10.50. Store **1050 paise** — an integer. The smallest unit of a currency (paise, cents) is called the **minor unit**.

```
₹10.50  -->  store 1050   (paise)
$3.99   -->  store 399    (cents)
```

Why this works: integers in a computer are exact. `1050 + 399` is always exactly `1449`, no drift, ever. Use a 64-bit integer (`int64` in Go, `BIGINT` in SQL).

### Non-intuitive case: not every currency has 2 decimal places

If your code converts back to display amounts with "divide by 100" everywhere, it's wrong for some currencies:

| Currency | Decimal places | Stored `1050` means |
|---|---|---|
| INR, USD | 2 | ₹10.50 (1050 ÷ 100) |
| Japanese yen (JPY) | 0 — yen has no sub-unit | ¥1050 (no division!) |
| Bahraini dinar (BHD) | 3 | BD 1.050 (1050 ÷ 1000) |

A hardcoded ÷100 would show a Japanese customer ¥10.50 instead of ¥1050 — a 100× billing error. The standard list of how many decimals each currency uses is called **ISO 4217**; money libraries ship it built in.

**Rule: always store the currency code next to the amount** — `(1050, "INR")` — so the code can look up the right number of decimals.

### Non-intuitive case: parsing input

Never do: user types `"10.50"` → convert to float `10.5` → multiply by 100. The float step can produce `1049.9999…` which truncates to **1049**.

Instead, parse the *string* directly: split on the dot, `"10"` and `"50"` → `10 × 100 + 50 = 1050`. Pure integer math, no float ever created. (Most money libraries do this for you.)

### Non-intuitive case: JSON and APIs

In most JSON parsers, every number is a float. If your API sends `{"amount": 10.50}`, the receiving side gets a float and the damage is done.

**Send money as an integer of minor units or as a string:**
```json
{"amount_minor": 1050, "currency": "INR"}     // good
{"amount": "10.50", "currency": "INR"}        // also good
{"amount": 10.50}                             // bad — it's a float now
```

---

## 3. When integers aren't enough: decimal types

Adding and subtracting whole paise is exact. But some calculations land **between** paise mid-way:

- **Interest rates:** 4.35% yearly = 0.00011917…% daily. One day's interest on ₹10,000 = 119.178 paise. Not a whole number.
- **Currency conversion:** $12.34 at a rate of 83.2467 = 1027.264… paise.
- **Splitting/proration:** ₹997 used for 11 of 30 days = 36556.33… paise.

For these, use a **decimal type**: Java `BigDecimal`, Python `decimal.Decimal`, Go `shopspring/decimal`, SQL `NUMERIC`.

**"Decimal" vs "float" — aren't they the same thing?** Both hold numbers with a fractional part, but they store them differently, and that's the whole point:

- A **float** stores the number in **base 2** (sums of halves, quarters, eighths…). `0.5` is fine (it's ½), but `0.1` has no exact binary form — the hardware stores `0.1000000000000000055511…` and there is nothing you can do about it. It's one fixed-size box (64 bits), fast because the CPU does it natively.
- A **decimal type** stores the number in **base 10** — effectively the digits you wrote, like `(digits=1050, point after 2 digits)`. `0.1` is stored as exactly `0.1`. It's implemented in software, so it's slower and can grow as big/precise as needed — which is fine, because money code doesn't need raw speed, it needs exactness.

Rough analogy: a float is the number translated into a language (binary) that can't spell most decimal words, so it writes the closest word it has. A decimal type just keeps your original spelling.

Two rules:

1. **Create decimals from strings or integers, never from floats.** `Decimal("0.1")` is exact; `Decimal(0.1)` copies the float's error in.
2. **Do the messy middle math in decimals, round back to whole paise once at the end.** Inputs are money (whole paise), outputs are money — only the middle is fractional.

### Under the hood: a tiny shopspring/decimal

A decimal type is not magic — it's about 100 lines of integer math. Here is a minimal Go implementation that mirrors how `shopspring/decimal` actually works inside. The whole trick is one struct:

```go
package decimal

import (
	"fmt"
	"math/big"
	"strings"
)

// Decimal represents the exact number value × 10^exp.
// 10.50 is stored as (value=1050, exp=-2) — literally the digits you
// wrote plus a note of where the point sits. No binary fractions, ever.
//
// How big.Int holds the digits internally:
//
//	type Int struct {
//	    neg bool // sign, kept separately
//	    abs nat  // the magnitude
//	}
//	type nat []Word // Word = one 64-bit machine word
//
// So the magnitude is a SLICE of 64-bit words — the number written in
// base 2^64, least-significant word first. Same idea as decimal digits:
// in base 10, 1050 is the digit list [0, 5, 0, 1] (ones first), where
// each digit holds 0..9 and position i is worth 10^i. big.Int does the
// identical thing with giant digits: each word holds 0..2^64-1 and
// position i is worth (2^64)^i. The value is always:
//
//	abs[0]×(2^64)^0 + abs[1]×(2^64)^1 + abs[2]×(2^64)^2 + …
//
// HOW the split into words is computed: repeated divide-by-the-base,
// keeping the remainders. It's the same procedure you'd use to read
// the decimal digits off 1050 by dividing by 10:
//
//	1050 ÷ 10 = 105 remainder 0   -> digit[0] = 0   (ones)
//	 105 ÷ 10 =  10 remainder 5   -> digit[1] = 5   (tens)
//	  10 ÷ 10 =   1 remainder 0   -> digit[2] = 0   (hundreds)
//	   1 ÷ 10 =   0 remainder 1   -> digit[3] = 1   (thousands)
//	quotient hit 0 -> stop. digits = [0, 5, 0, 1] = "1050" reversed ✓
//
// Each remainder is what's too small to be divided out (always < the
// divisor, so it fits one digit); each quotient is what's left for the
// higher positions. big.Int words are the same loop with 2^64 as the
// divisor. Trace section 5's overflowing intermediate through it
// (2^64 = 18446744073709551616):
//
//	n = 125000000000000000000                       (1.25×10^20)
//
//	step 1:  125000000000000000000 ÷ 2^64
//	         = quotient 6, remainder 14319535557742690304
//	           (because 6 × 2^64 = 110680464442257309696, and
//	            125000000000000000000 − 110680464442257309696
//	            = 14319535557742690304, which is < 2^64 ✓)
//	         -> abs[0] = 14319535557742690304
//
//	step 2:  6 ÷ 2^64 = quotient 0, remainder 6
//	         -> abs[1] = 6
//
//	quotient hit 0 -> stop.  abs = [14319535557742690304, 6]
//
//	read it back with the formula above to check:
//	    14319535557742690304 × (2^64)^0     =  14319535557742690304
//	  +                    6 × (2^64)^1     = 110680464442257309696
//	                                          ─────────────────────
//	                                          125000000000000000000 ✓
//
// So "which word gets which value" is never a choice — word i is
// (n ÷ (2^64)^i) mod 2^64, i.e. the i-th remainder of that loop, and
// the slice length is however many steps until the quotient hits 0.
//
// Worked examples (2^64 = 18446744073709551616 ≈ 1.8×10^19):
//
//	1050                  -> abs = [1050]
//	                         1 word — anything below 2^64 is one "digit"
//
//	10^19 paise           -> abs = [10000000000000000000]
//	                         still 1 word! 10^19 overflows int64
//	                         (max ~9.2×10^18) but a word is UNSIGNED,
//	                         so one word holds up to ~1.8×10^19 —
//	                         the sign lives in `neg`, not in the words
//
//	2^64                  -> abs = [0, 1]
//	                         the base itself = "10" in base 2^64,
//	                         exactly like ten = digits [0, 1] in base 10
//	                         (0×1 + 1×2^64)
//
//	2^64 + 5              -> abs = [5, 1]
//	                         (5×1 + 1×2^64) — low word first
//
//	1.25×10^20            -> abs = [14319535557742690304, 6]
//	(section 5's              i.e. 14319535557742690304×1 + 6×2^64
//	 overflowing              = 125000000000000000000 ✓ — the very
//	 fee intermediate,        product that wraps an int64 into garbage
//	 5×10^16 × 2500)          is just a 2-word slice here
//
//	2^100                 -> abs = [0, 68719476736]
//	                         0×1 + 68719476736×2^64 (= 2^36×2^64) —
//	                         like writing 10^9 in decimal: a 1 in a
//	                         high position, zeros below it
//
// When a result needs more words, the slice is reallocated longer.
// That is the entire "can't overflow" property: int64 is one fixed
// 64-bit box, big.Int is as many boxes as the number needs. Arithmetic
// then works like school arithmetic on these big digits — add word by
// word carrying overflow into the next word, multiply word by word —
// done in software, which is why it's slower than native int64 and why
// we only use it where exactness matters (money) or values can exceed
// 64 bits (section 5's intermediates and sums).
type Decimal struct {
	value *big.Int
	exp   int32
}

// New returns value × 10^exp. New(1050, -2) is exactly 10.50.
func New(value int64, exp int32) Decimal {
	return Decimal{value: big.NewInt(value), exp: exp}
}

// NewFromString parses "10.50" with pure string/integer work — exactly
// the "split on the dot" rule from section 2. No float is ever created.
func NewFromString(s string) (Decimal, error) {
	intPart, fracPart, _ := strings.Cut(s, ".")
	digits := intPart + fracPart // "10" + "50" -> "1050"
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Decimal{}, fmt.Errorf("can't convert %q to decimal", s)
	}
	return Decimal{value: v, exp: -int32(len(fracPart))}, nil
}

// pow10 returns 10^n as a big.Int.
func pow10(n int32) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// rescale rewrites d with a smaller exponent, same numeric value:
// 10.5 = (105, -1) rescaled to exp -2 becomes (1050, -2). It only pads
// with zeros — that's why nothing is ever lost doing it.
func (d Decimal) rescale(exp int32) Decimal {
	if d.exp == exp {
		return d
	}
	mul := pow10(d.exp - exp)
	return Decimal{value: new(big.Int).Mul(d.value, mul), exp: exp}
}

// Add: line the decimal points up (rescale both to the finer exponent),
// then it's plain integer addition. 0.1 + 0.2 = (1,-1)+(2,-1) = (3,-1).
func (d Decimal) Add(d2 Decimal) Decimal {
	exp := min(d.exp, d2.exp)
	a, b := d.rescale(exp), d2.rescale(exp)
	return Decimal{value: new(big.Int).Add(a.value, b.value), exp: exp}
}

// Sub is Add with the sign flipped.
func (d Decimal) Sub(d2 Decimal) Decimal {
	exp := min(d.exp, d2.exp)
	a, b := d.rescale(exp), d2.rescale(exp)
	return Decimal{value: new(big.Int).Sub(a.value, b.value), exp: exp}
}

// Mul: multiply the digits, add the exponents — school arithmetic.
// 10.50 × 0.025 = (1050,-2)×(25,-3) = (26250, -5) = 0.26250. Exact.
func (d Decimal) Mul(d2 Decimal) Decimal {
	return Decimal{
		value: new(big.Int).Mul(d.value, d2.value),
		exp:   d.exp + d2.exp,
	}
}

// DivisionPrecision: division (1/3 = 0.333…) can't always be exact, so
// we keep this many fractional digits — same default as shopspring.
var DivisionPrecision = int32(16)

// Div: shift the numerator left by DivisionPrecision digits first
// ("multiply before divide", section 4), then integer-divide.
func (d Decimal) Div(d2 Decimal) Decimal {
	num := new(big.Int).Mul(d.value, pow10(DivisionPrecision))
	quo := new(big.Int).Quo(num, d2.value)
	return Decimal{value: quo, exp: d.exp - d2.exp - DivisionPrecision}
}

// Cmp compares two decimals: -1 if d < d2, 0 if equal, +1 if d > d2.
// Note 1.50 == 1.5: (150,-2) and (15,-1) rescale to the same thing.
func (d Decimal) Cmp(d2 Decimal) int {
	exp := min(d.exp, d2.exp)
	return d.rescale(exp).value.Cmp(d2.rescale(exp).value)
}

// Equal is the comparison floats can't do reliably (section 1).
func (d Decimal) Equal(d2 Decimal) bool { return d.Cmp(d2) == 0 }

// Round rounds to `places` digits after the point, half-up
// (102.5 -> 103). The dropped digits are inspected as an integer
// remainder — no float sneaks in even here.
func (d Decimal) Round(places int32) Decimal {
	if -d.exp <= places {
		return d // already coarse enough
	}
	div := pow10(-d.exp - places)
	quo, rem := new(big.Int).QuoRem(d.value, div, new(big.Int))
	rem.Abs(rem).Mul(rem, big.NewInt(2)) // compare 2×rem vs div ⇔ rem vs ½
	if rem.Cmp(div) >= 0 {
		bumpAwayFromZero(quo, d.value.Sign())
	}
	return Decimal{value: quo, exp: -places}
}

// RoundBank rounds half-even — "banker's rounding", the accounting
// standard from section 4: exact .5 ties go to the nearest even digit
// (102.5 -> 102, 103.5 -> 104), so the bias cancels out over millions
// of operations.
func (d Decimal) RoundBank(places int32) Decimal {
	if -d.exp <= places {
		return d
	}
	div := pow10(-d.exp - places)
	quo, rem := new(big.Int).QuoRem(d.value, div, new(big.Int))
	rem.Abs(rem).Mul(rem, big.NewInt(2))
	c := rem.Cmp(div)
	// bump if over half, or exactly half and the kept digit is odd
	if c > 0 || (c == 0 && quo.Bit(0) == 1) {
		bumpAwayFromZero(quo, d.value.Sign())
	}
	return Decimal{value: quo, exp: -places}
}

func bumpAwayFromZero(quo *big.Int, sign int) {
	if sign < 0 {
		quo.Sub(quo, big.NewInt(1))
	} else {
		quo.Add(quo, big.NewInt(1))
	}
}

// IntPart returns the whole-number part as int64, dropping any fraction
// — use it to get back to integer minor units after rounding.
func (d Decimal) IntPart() int64 {
	if d.exp >= 0 {
		return new(big.Int).Mul(d.value, pow10(d.exp)).Int64()
	}
	return new(big.Int).Quo(d.value, pow10(-d.exp)).Int64()
}

// String prints the exact value: re-insert the point -exp digits from
// the right. (1050, -2) -> "10.50".
func (d Decimal) String() string {
	if d.exp >= 0 {
		return new(big.Int).Mul(d.value, pow10(d.exp)).String()
	}
	abs := new(big.Int).Abs(d.value).String()
	frac := int(-d.exp)
	if len(abs) <= frac {
		abs = strings.Repeat("0", frac-len(abs)+1) + abs // "3" -> "03" for 0.3
	}
	point := len(abs) - frac
	s := abs[:point] + "." + abs[point:]
	if d.value.Sign() < 0 {
		s = "-" + s
	}
	return s
}
```

What to notice:

- **Every operation is integer math on the digits.** Add/Sub line the points up by padding zeros (exact), Mul multiplies digits and adds exponents (exact). The only place precision can be lost is `Div` — and there it's *controlled*: you choose how many digits to keep, instead of binary deciding for you.
- **There is no float anywhere in the file.** Even rounding decides "is the dropped part ≥ half?" by comparing two integers.
- **This is genuinely how `shopspring/decimal` works** — its struct is the same `(value *big.Int, exp int32)` pair. The real library adds what production needs: `NewFromFloat` (avoid it — rule 1), more rounding modes, JSON/SQL marshalling, and years of edge-case fixes. Use the real one; build the toy one once to stop it being magic.

### Using it

The same scenarios from this doc, written with the package above (the real `shopspring/decimal` has identical call shapes):

```go
// 1. The section-1 example, fixed.
a, _ := decimal.NewFromString("0.1")
b, _ := decimal.NewFromString("0.2")
sum := a.Add(b)
fmt.Println(sum)                            // 0.3 — exactly, not 0.30000000000000004
fmt.Println(sum.Equal(decimal.New(3, -1)))  // true — comparisons work again

// 2. The ₹41 fee from section 4: 250 bps on 4100 paise.
amount := decimal.New(4100, 0)              // 4100 paise — created from an integer
rate := decimal.New(250, -4)                // 250 bps = exactly 0.0250
fee := amount.Mul(rate)                     // 102.5000 — exact intermediate, no order trap
fmt.Println(fee.Round(0))                   // 103 (half-up)
fmt.Println(fee.RoundBank(0))               // 102 (banker's: the .5 tie goes to even)

// 3. One day of interest on ₹10,000 at 4.35%/year (section 3's example).
principal := decimal.New(1_000_000, 0)      // ₹10,000 in paise
yearly, _ := decimal.NewFromString("0.0435")
daily := yearly.Div(decimal.New(365, 0))    // 0.0001191780821917808… (16 digits kept)
interest := principal.Mul(daily)            // 119.1780821917808…
fmt.Println(interest.RoundBank(0))          // 119 — rounded ONCE, at the end (rule 2)

// 4. Back to int64 minor units for storage — decimals live only in the middle.
paise := interest.RoundBank(0).IntPart()    // int64(119), ready for the BIGINT column

// 5. Books must balance (section 4): round one side, derive the other.
gross := decimal.New(10005, 0)              // ₹100.05 in paise
feeR := gross.Mul(decimal.New(250, -4)).RoundBank(0) // 250.125 -> 250
net := gross.Sub(feeR)                      // 9755 — derived, not rounded again
fmt.Println(net.Add(feeR).Equal(gross))     // true — reconciliation holds
```

With the real library, swap the import for `github.com/shopspring/decimal` — `New`, `NewFromString`, `Add`, `Sub`, `Mul`, `Div`, `Round`, `RoundBank`, `Cmp`, `Equal`, `IntPart`, `String` all exist there with the same signatures. The two rules above still apply: never construct from a float, and round back to whole minor units exactly once.

---

## 4. Fee calculations

### Store percentages as whole numbers too

`2.5%` stored as the float `0.025` has the same binary problem as any float. The standard fix: **basis points** ("bps") — one basis point = 0.01%. So:

```
2.5%  =  250 bps      (an exact integer)
fee   =  amount × 250 / 10000
```

### Multiply first, divide last

Division is where fractions (and therefore precision loss) appear, so push it to the end.

**Non-intuitive example** — fee of 250 bps on ₹41 (4100 paise):

```
Wrong:  4100 / 10000 = 0 (integer division!) → 0 × 250 = 0 paise fee
Right:  4100 × 250 = 1,025,000 → / 10000 = 102.5 → round → 103 paise
```

Same numbers, same formula, opposite order — one gives a free transaction.

### Round once, with a deliberate rule

Rounding twice creates errors. And "round" itself is ambiguous when the digit is exactly 5 — what is 102.5?

- **Half-up:** 102.5 → 103. What people expect, but over millions of fees it systematically rounds *up* more than down — a measurable bias in someone's favor.
- **Half-even ("banker's rounding"):** ties go to the nearest *even* number: 102.5 → 102, but 103.5 → 104. Up and down cancel out over many operations. This is the accounting standard.
- **Always-up or always-down:** a business choice ("fees round in our favor", "refunds round in the customer's favor"). Fine — but pick it on purpose and write it down.

**Non-intuitive trap:** built-in `round()` differs across languages. Python uses half-even (`round(2.5)` is `2`, not `3`); many others use half-up. Never rely on the default — specify the mode.

### The books must balance: never round both sides

Customer pays ₹100.05 (gross), fee is 250 bps:

```
fee = 10005 × 250 / 10000 = 250.125 → round → 250 paise
```

Now **derive** the other side by subtraction — don't compute it independently:

```
net = 10005 − 250 = 9755          ✓  9755 + 250 = 10005, balances

Wrong way: also round net from a formula → both sides rounded
independently can give net + fee = 10004 or 10006. A cent appears
or vanishes from thin air, and reconciliation fails.
```

**Rule: round one value, subtract to get the other. `net + fee == gross` must hold exactly, always.**

### Non-intuitive case: splitting money N ways

Split ₹100 (10000 paise) among 3 people. Each share is 3333.33… paise.

```
Round each share independently: 3333 × 3 = 9999  →  1 paisa vanished
```

Correct approach (**largest-remainder method**): give everyone the rounded-down share, then hand the leftover paise out one at a time, in a fixed deterministic order (e.g., to the shares with the biggest remainders, ties broken by index):

```
3333 + 3333 + 3334 = 10000  ✓
```

"Deterministic" matters: re-running the split must give the identical result, or retries and audits disagree.

### Multi-part fees: order matters

Say a fee is "2.5%, minimum ₹5, plus 18% GST on the fee". These give different answers:

```
A) percentage → apply minimum → tax on capped fee
B) percentage → tax → apply minimum to the total
```

Neither is "mathematically correct" — it's a business/regulatory decision. Pick the order, document it, and apply it identically everywhere. Compute each component separately, round each by your policy, sum, and verify against the gross.

### Freeze the fee with the transaction

Store the computed fee *and the rate used* on the transaction record. If you recompute later "from the current rate," and the rate changed in between, history rewrites itself. Fee math should also be a **pure function**: same inputs → same output, every time, so a retried request can't produce a different fee.

---

## 5. Very large amounts: a different failure mode

Small amounts fail by precision drift. Large amounts fail by **overflow** — an integer exceeding what its type can hold. `int64` tops out around 9.2 × 10^18 (about ₹92,000 trillion in paise). "We'll never have amounts that big" — true, but that's not where it bites:

### Non-intuitive case: the intermediate overflows, not the amount

```
fee = amount × bps / 10000
```

Take amount = 5 × 10^16 paise (huge but fits in int64 fine) and bps = 2500:

```
5×10^16 × 2500 = 1.25×10^20   ← exceeds int64 max (9.2×10^18). Overflow.
```

The input fits. The final fee (1.25 × 10^16) fits. Only the **multiplication in the middle** doesn't. Fixes:

- Do the multiply in a wider type: 128-bit integers, `BigInteger`, or a decimal type — then divide and convert back.
- Or set a hard business cap (next point) low enough that the product provably fits.

**Worked example of "use a wider type".** The idea: the danger zone is only the two steps `× then ÷`. So step *into* a bigger number type just for those steps, then step back out — the final result is small enough to fit in `int64` again.

Go, using `math/big.Int` (a "big integer" can hold numbers of any size):

```go
import "math/big"

func feeMinor(amount, bps int64) int64 {
    p := new(big.Int).Mul(big.NewInt(amount), big.NewInt(bps)) // can't overflow
    p.Div(p, big.NewInt(10_000))                               // back under int64 max
    return p.Int64()                                           // safe to narrow now
}

// amount = 5×10^16, bps = 2500:
//   plain int64:  5×10^16 × 2500 wraps around → garbage (even negative)
//   big.Int:      product = 1.25×10^20 (fine, big.Int grows as needed)
//                 ÷ 10000 = 1.25×10^16 → fits in int64 → return it
```

Java, same shape with `BigInteger`:

```java
long fee = BigInteger.valueOf(amount)
        .multiply(BigInteger.valueOf(bps))
        .divide(BigInteger.valueOf(10_000))
        .longValueExact();   // throws instead of silently truncating — good
```

Python note: Python's `int` is already arbitrary-size — `5*10**16 * 2500` just works. Overflow is a static-typed-language problem (Go, Java, C, Rust).

**Without `math/big` — option 1: split the amount (no imports).** Since `amount = q×10000 + r`, algebra gives `amount×bps/10000 = q×bps + r×bps/10000`, and each piece stays small:

```go
func feeMinor(amount, bps int64) int64 {
    q := amount / 10_000
    r := amount % 10_000        // always < 10000, so r×bps is tiny
    return q*bps + r*bps/10_000 // exact — same result as the one-shot formula
}
```

This is **not** the naive "divide first" (`amount/10000*bps` alone loses the remainder — ₹41 at 250 bps becomes 0 again). Keeping both the quotient *and* remainder terms preserves exactness. Safe as long as `q×bps` fits in int64 — guaranteed by your business cap.

**Without `math/big` — option 2: `math/bits` 128-bit primitives.** The CPU can return the full 128-bit product of two 64-bit numbers as two halves:

```go
import "math/bits"

func feeMinor(amount, bps uint64) uint64 {
    hi, lo := bits.Mul64(amount, bps)       // full product = hi×2^64 + lo
    quo, rem := bits.Div64(hi, lo, 10_000)  // divides the 128-bit value
    _ = rem                                 // apply your rounding rule with rem
    return quo
}
```

Caution: `bits.Div64` panics if the quotient exceeds 64 bits (`hi >= 10000`) — your input cap should rule that out, and panicking is at least loud, not silent. These are `uint64`: validate amounts are non-negative before converting.

Two details: `Div`/`divide` here **truncates** (drops the fraction) — apply your real rounding rule on the remainder if you need half-even. And when narrowing back, prefer a method that fails loudly (`longValueExact`) or check the bound yourself; `big.Int.Int64()` silently mangles values that don't fit.

### Overflow is silent — and silently wrong

In most languages, unchecked integer overflow doesn't crash. It **wraps around**: a giant positive number suddenly becomes negative or tiny, and the code keeps running with garbage. A crash would be better — at least you'd notice.

Use checked arithmetic, or pre-validate: before computing `a × b`, check `a <= MAX / b` and reject the transaction loudly if it fails.

### Set a business maximum at the front door

Decide an explicit max amount (say 10^15 paise) and reject anything larger at the API boundary. Benefits: an "impossible" overflow deep in fee math becomes a clean validation error at the edge, and you can *prove* your headroom — if amount ≤ 10^15 and bps ≤ 10^5, the product ≤ 10^20, so you know you need 128-bit there.

### Non-intuitive case: sums overflow even when every row is small

Every transaction fits in `int64`, but `SUM()` over a billion of them for a settlement total might not. Use `NUMERIC` for SQL aggregates that could exceed the column type, and big-integer accumulators in code.

**Worked example.** Say transactions average ₹10 crore (10^10 paise — every row fits easily in `int64`, whose max is ~9.2×10^18). Sum one billion (10^9) of them:

```
10^10 paise per row × 10^9 rows = 10^19   ← bigger than int64 max. Overflow.
```

No single row is anywhere near the limit — only the running total is. The same trap hits much sooner with smaller column types: an `INT` (32-bit) column maxes at ~2.1×10^9 paise ≈ ₹2.15 crore, so a day's `SUM()` of ordinary payments can overflow it.

In SQL, whether you're safe depends on the database:

```sql
-- SQL Server: SUM(bigint) returns bigint → can throw
--   "Arithmetic overflow error". Fix: widen inside the SUM:
SELECT SUM(CAST(amount_minor AS DECIMAL(38,0))) FROM transactions;

-- PostgreSQL: SUM(bigint) already returns NUMERIC → safe by default.
-- MySQL: SUM() on integers returns DECIMAL → safe by default.
```

Don't memorize the table — just never assume the aggregate is as safe as the column; check what type your database's `SUM()` returns.

In application code, the same fix as fee math — accumulate in a big-integer:

```go
total := new(big.Int)                       // grows as needed, can't overflow
for _, tx := range transactions {
    total.Add(total, big.NewInt(tx.AmountMinor))
}
```

A plain `var total int64` loop would wrap around silently — the settlement report would show a wrong (possibly negative) total with no error anywhere.

### Non-intuitive case: JavaScript loses cents above 2^53

JS has no integer type — `Number` is a float. Above 2^53 (≈9 × 10^15) it can't represent every whole number:

```js
9007199254740992 + 1  ==>  9007199254740992   // adding 1 does nothing
```

Adding one paisa to a large enough balance is literally a no-op. Use `BigInt` in JS, and strings in JSON for amounts that might get that large.

### Don't retreat to float for big numbers

Floats are tempting at scale because they never "overflow." But they have a different flaw: **a float only keeps the first ~16 digits of a number.** It works like scientific notation:

```
1.234567890123456 × 10^n
└── ~16 digits ──┘    └── makes it big ──┘
```

The `× 10^n` part can scale the number up to ~10^308 — that's why it never overflows — but it adds **size, not digits**. Anything past the 16th digit is rounded off the moment the value is stored.

**Example** — store this balance in a float64:

```
your number:    10,000,000,000,000,000,001  paise    (20 digits)
float keeps:     1.000000000000000 × 10^19           (only ~16 digits fit)
stored value:   10,000,000,000,000,000,000  paise    ← the final 1 is gone
```

No error, no warning — the paisa vanished at storage time, before any math even ran. And adding 1 to that balance does nothing, forever: the result keeps rounding back to the same 16-digit value.

**Why the loss gets worse as numbers grow.** Internally the float works in binary. The rule is simple: **it keeps the top 53 binary digits of your number** (that's where the "~16 decimal digits" comes from) **plus an exponent `× 2^n`** recording where those digits sit. Any digit past the 53rd is rounded away.

Here is what the 64 bits of a float64 actually contain:

```
┌──────┬─────────────┬──────────────────────────────────────────────┐
│ sign │  exponent   │  fraction                                    │
│ 1 bit│  11 bits    │  52 bits                                     │
└──────┴─────────────┴──────────────────────────────────────────────┘
sign:     0 = positive, 1 = negative
exponent: the n of "× 2^n", stored with 1023 added to it
          (so n = 15 is stored as 15 + 1023 = 1038).
          Why +1023: the 11-bit field only holds 0..2047, but n can be
          negative (tiny values like 2^-10). Adding the midpoint, 1023,
          shifts the range so n = -1022..+1023 all fit as non-negatives.
fraction: the window digits AFTER the leading "1." — the leading 1
          itself is not stored: every binary number starts with 1,
          so it's implied for free (1 implied + 52 stored = the
          53-digit window)
```

**What "the leading 1" means** — trace one number through. 50,000 paise in binary is `1100001101010000`. To store it, the float rewrites it in scientific notation: slide the point left until exactly **one digit is left in front of it**. That front digit is "the leading 1":

```
plain binary:    1100001101010000              (50,000 paise)

rewritten:       1.100001101010000 × 2^15      (point slid 15 places left)
                 ↑ └─────────────┘
          the leading 1:   the rest of the digits
          the number's
          FIRST binary
          digit

what gets stored where:
  exponent field:  15 + 1023 = 1038
  fraction field:  100001101010000 0…0         (the digits after the point)
  the leading 1:   NOWHERE — in binary the front digit is always 1,
                   so the reader just re-attaches "1." when decoding
```

So whenever the examples below say "after the leading 1.", they mean: the number's first binary digit is chopped off (it's always 1, nothing is lost), and only the remaining digits go into the 52-bit fraction field.

The same rule applied to three real balances:

**₹500.00** = 50,000 paise = `1100001101010000` in binary (16 digits). Fits in the window with room to spare:

```
your number:   1100001101010000                                       (16 digits)
float keeps:  [1100001101010000                                     ]
               └────────────────── 53-digit window ─────────────────┘
stored as:     1.100001101010000 × 2^15            (binary form)
          =    5.000000000000000 × 10^4            (same value in decimal)
          =    50,000 paise — exact ✓

all 64 bits:   0 10000001110 1000011010100000000000000000000000000000000000000000
               ↑ └─────────┘ └────────────────────────────────────────────────┘
            sign=+  15+1023    "100001101010000" (after the leading 1.) + 37 zeros
                    =1038
```

**9,007,199,254,740,993 paise** (≈9×10^15; this is 2^53 + 1) — in binary a `1`, then 52 zeros, then a `1`: 54 digits, one too many:

```
your number:   1000000000000000000000000000000000000000000000000000 0 1   (54 digits)
float keeps:  [1000000000000000000000000000000000000000000000000000 0] ↑
               └────────────────── 53-digit window ───────────────────┘ lost
stored as:     1.0000000000000000000000000000000000000000000000000000 × 2^53   (binary form)
          =    9.007199254740992 × 10^15           (same value in decimal — 16 digits, full)
          =    9,007,199,254,740,992 paise   ← 1 paisa short, no error raised

all 64 bits:   0 10000110100 0000000000000000000000000000000000000000000000000000
               ↑ └─────────┘ └────────────────────────────────────────────────┘
            sign=+  53+1023    52 zeros (after the leading 1.) — the trailing 1
                    =1076      of your number is nowhere in these bits
```

The digit that fell off was the **1-paisa digit** — from this size on, the float can only store *even* numbers of paise; odd amounts snap to a neighbor.

**10,000,000,000,000,000,001 paise** (the 10^19 + 1 example from above) — 64 binary digits, eleven too many:

```
your number:   10001010110001110010001100000100100010011110100000000 00000000001
float keeps:  [10001010110001110010001100000100100010011110100000000] └─ 11 lost ─┘
               └────────────────── 53-digit window ─────────────────┘
stored as:     1.0001010110001110010001100000100100010011110100000000 × 2^63   (binary form)
          =    1.000000000000000 × 10^19           (same value in decimal — the +1 would
                                                    need a 20th digit; there's no room)
          =    10,000,000,000,000,000,000 paise   ← the final 1 is gone

all 64 bits:   0 10000111110 0001010110001110010001100000100100010011110100000000
               ↑ └─────────┘ └────────────────────────────────────────────────┘
            sign=+  63+1023    the 52 digits after the leading 1. — the 11 lost
                    =1086      digits of your number are nowhere in these bits
```

The 11 lost digits could have held anything from 0 to 2,047 paise (~₹20) — all of that detail is rounded away, so at this size the float can only store amounts ~₹20 apart.

**Summary — the only balances a float64 can actually hold, at each size** (in paise):

```
around 50,000 (₹500):        … 49,999   50,000   50,001   50,002 …
                             every whole number storable — exact

around 9,007,199,254,740,992: … 740,990   740,992   740,994   740,996 …
                             only even numbers storable — gap of 2 paise

around 10^19:                … 9,999,999,999,999,997,952
                               10,000,000,000,000,000,000
                               10,000,000,000,000,002,048 …
                             gap of 2,048 paise ≈ ₹20 between storable values
```

A float asked to hold any balance *not* on its list silently keeps the nearest listed value instead. Example: deposit ₹5 (500 paise) into the 10^19 balance — the true result `…000,000,500` is not on the list, the nearest listed value is `…000,000,000`, so the balance doesn't change and the deposit vanishes.

An `int64` has no such list — every whole number up to its max is storable, exactly.

So the two types fail in opposite ways:

- **int64**: every whole number is a tick, all the way up — then one hard, *detectable* stop (overflow).
- **float64**: no stop ever, but past ~16 digits the value becomes silently *approximate* — totals look plausible and are slightly wrong, and nothing flags it.

For money, quiet-and-wrong is worse than loud-and-broken. That's why "it won't overflow" is not a reason to use float for big amounts.

---

## 6. Quick checklist

- [ ] Money stored as `int64` minor units (paise/cents), never float
- [ ] Currency code stored next to every amount; decimals looked up per currency (ISO 4217)
- [ ] Input strings parsed straight to integers — no float hop
- [ ] APIs/JSON carry money as integers or strings, never JSON numbers with decimals
- [ ] Percentages stored as integer basis points
- [ ] Multiply before divide; round once, at the end
- [ ] Rounding mode chosen explicitly and documented (half-even unless business says otherwise)
- [ ] One side rounded, other side derived: `net + fee == gross` exactly
- [ ] Splits use largest-remainder method — no vanishing cents
- [ ] Order of fee components (caps, taxes) documented and consistent
- [ ] Fee + rate snapshot stored on the transaction
- [ ] `amount × rate` done in 128-bit/big-integer; checked, not silent, overflow
- [ ] Explicit max-amount limit enforced at the API edge
- [ ] Aggregates (SUM, balances) use wide types
