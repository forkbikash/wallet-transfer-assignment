# Water Jug Puzzle (Aptitude Question)

## Question

You have two jars:

- **Jar 1:** 5 litres capacity
- **Jar 2:** 4 litres capacity

There is an unlimited supply of water. Neither jar has any measurement markings.
Using only these two jars, measure out **exactly 2 litres** of water.

(This is the classic *water jug problem* / *decanting puzzle* — famously featured
in *Die Hard 3* with a 5L and 3L jug to measure 4L. It also appears as
LeetCode 365, "Water and Jug Problem".)

## Answer

| Step | Action                                        | 5L jar | 4L jar |
|------|-----------------------------------------------|--------|--------|
| 1    | Fill the 5L jar                               | 5      | 0      |
| 2    | Pour from 5L into 4L until the 4L jar is full | 1      | 4      |
| 3    | Empty the 4L jar                              | 1      | 0      |
| 4    | Pour the 1L from the 5L jar into the 4L jar   | 0      | 1      |
| 5    | Fill the 5L jar again                         | 5      | 1      |
| 6    | Top up the 4L jar (takes 3L more)             | **2**  | 4      |

**Exactly 2 litres remain in the 5L jar.**

## The Math Behind It

- **What's measurable:** with jars of capacity `a` and `b`, you can measure any
  multiple of **gcd(a, b)**, up to `a + b`.
- Here gcd(5, 4) = 1, so **every whole number from 1 to 9 litres** is measurable.
- **Why it works:** every reachable amount has the form `5x + 4y` for integers
  `x`, `y` (fills and empties) — this is Bézout's identity, computable via the
  extended Euclidean algorithm.
- **In CS:** the puzzle is a standard example of **BFS / state-space search**,
  where each state is the pair `(litres in jar 1, litres in jar 2)`.

## Second Example: 6L and 15L Jars (gcd ≠ 1)

This pair shows what happens when the capacities are **not coprime**.

### What can we measure?

- gcd(6, 15) = **3**, so only **multiples of 3** are measurable: 3, 6, 9, 12, 15, 18, 21 litres.
- Amounts like 1L, 2L, 4L, or 7L are **impossible** — no sequence of fills,
  empties, and pours will ever produce them.

**Why impossible?** Every action (fill, empty, pour) changes the total water by
`±6` or `±15`, both multiples of 3. So the amount in any jar is always of the
form `6x + 15y = 3(2x + 5y)` — always divisible by 3. You can never escape
multiples of the gcd.

### Question: measure exactly 3 litres

| Step | Action                                          | 6L jar | 15L jar |
|------|-------------------------------------------------|--------|---------|
| 1    | Fill the 15L jar                                | 0      | 15      |
| 2    | Pour from 15L into 6L until the 6L jar is full  | 6      | 9       |
| 3    | Empty the 6L jar                                | 0      | 9       |
| 4    | Pour from 15L into 6L again                     | 6      | **3**   |

**Exactly 3 litres remain in the 15L jar.**

### The math check (Bézout's identity)

We need integers `x`, `y` with:

```
6x + 15y = 3
```

One solution: `x = -2`, `y = 1`, since `15·1 − 6·2 = 15 − 12 = 3`.

The signs tell you the recipe: **fill the 15L jar once** (`y = +1`) and
**empty the 6L jar twice** (`x = −2`) — exactly what steps 1–4 above do
(the 6L jar is filled-by-pouring and emptied two times).

### Contrast with the 5L/4L pair

| Pair      | gcd | Measurable amounts          |
|-----------|-----|-----------------------------|
| 5L & 4L   | 1   | every integer 1–9           |
| 6L & 15L  | 3   | only multiples of 3, 3–21   |

The smaller the gcd, the finer the "resolution" of what you can measure;
coprime jars (gcd = 1) give you everything.
