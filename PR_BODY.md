## What

Turn operating costs into **budgets** with actuals tracked from the ledger, and add **spending accounts** that are excluded from net worth and project funding.

## Why

Operating costs were fixed planning figures only. Now each can be a budget you track against real spending (e.g. Groceries €400 → €505 spent this month → over budget), and day-to-day spending money can live in dedicated accounts that don't inflate net worth or get counted as project funding.

## Budgets vs actuals

- `operating_costs.category_id` links a cost to a transaction category. The Costs page shows each tracked budget's **actual-this-month vs budget** with a % bar, a **Budget alerts** panel, and **dashboard banners** — alerting at **≥90% (running low)** and **over budget**.
- Pure `model.BudgetStatus(actual, budget)` → percent + state (`ok` / `low` / `over`). Actuals from `Store.CategorySpend` (EUR-converted, expense, transfers excluded).
- Costs with no linked category stay plain fixed costs.

## Spending accounts

- `accounts.spending` flag. Spending accounts fund operating costs and are **excluded from `ComputeNAV`**, so projects / forecast / burn-down (which read NAV) exclude them too.
- Budget actuals scope to spending accounts when any are flagged, else all accounts (`CountSpendingAccounts`).
- Account form checkbox; accounts page tags them and shows a "counted in net worth" subtotal.

## Reviewer notes

- Both new columns are additive via `ensureColumn` (+ `schema.sql` for fresh DBs).
- Actuals only match **categorized** transactions — a budget tracks a transaction category, so grocery spends need that category set.
- `model.NullID` added to keep `database/sql` out of the controller layer.

## Tests

- `TestBudgetStatus` (incl. the over-budget groceries case) and `TestSpendingAccountsAndCategorySpend` (NAV exclusion + scoped-vs-all-account actuals using the 150/300/55 example), plus template render cases. `go build` / `vet` / `test ./...` green, gofmt clean.
