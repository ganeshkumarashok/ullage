# Costs

The built-in prices are approximate list prices, and they are wrong for almost
everyone. Reservations, savings plans, spot and negotiated discounts all move
them, often by more than half.

Treat the money as a way to rank findings against each other, not as a figure to
put in a budget.

- `--pricing FILE` supplies your own rates.
- `--no-cost` drops money from the output entirely.

Every report names the rate source it used, so a reader can tell whether a
number came from finance or from a built-in guess.

Rates are never blended across models. An H100 and a T4 differ by roughly
tenfold, so a single averaged rate would be made up.
