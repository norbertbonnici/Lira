# CLAUDE.md

Guidance for working on **Lira** in this repo. Read this before making changes.

## What this is
Lira is a small, self-hosted personal-finance web app (net worth, accounts,
transactions, investments) for a two-person household, designed to run on a
ZimaBoard. Go, server-rendered HTML, local SQLite, single static binary.
Licensed **AGPL-3.0-only**. `AGENTS.md` is the short generic-agent summary;
this file is the canonical, detailed guidance — keep both in sync when
commands or conventions change.

## Commands
- `make tidy` — `go mod tidy`. **Run first** (deps are not vendored; needs network).
- `make run` — `go run .` for local dev.
- `make build` — static binary `./lira` (`CGO_ENABLED=0`).
- `make linux` — cross-compile `./lira-linux-amd64` for the ZimaBoard (x86-64).
- `make deploy LIRA_HOST=user@your-server` — `make linux` + roll out via
  `deploy/deploy.sh` (stop → swap → start, backup + auto-rollback on the host).
- `go test ./...` — all tests (model: store + finance math; controller: TOTP RFC
  vectors, plan/ledger helpers; view: every template parses + executes).
- `go test ./internal/controller -run TestStillToDraw` — run one test by name.
- `go vet ./...` — vet.

Go **1.25+** required — follow `go.mod`'s `go` directive, which
`golang.org/x/crypto` bumps keep raising (currently v0.52 → go 1.25); the
net/http ServeMux method+pattern routing this app relies on needs ≥1.22.

Local HTTP smoke test: `LIRA_SECURE_COOKIES=false ./lira` → http://localhost:8080.
The flag must be off for plain HTTP, otherwise the session cookie isn't sent.

## Architecture — MVC, keep the layers clean
One-way dependency flow: `main → controller → {model, view}`. Model and view
never import each other. `internal/market` (external price/FX HTTP) is a separate
leaf — `main` wires it to the store; it imports neither model nor view (it defines
tiny local store interfaces instead), keeping network access out of the MVC layers.

- `main.go` — composition root: config, DB open, `store.Migrate()`, router,
  server, generic middleware (log / recover / security headers). Wires it all.
- `internal/config` — env configuration (`LIRA_*`).
- `internal/model` — **data only.** Domain types + `Store` (all SQL). Embeds
  `schema.sql`. No HTTP, no templating, no password/crypto (it only mints opaque
  session IDs via crypto/rand).
- `internal/view` — **presentation only.** Embeds templates + static assets;
  `View.Render(w, name, data)` writes HTML; display funcs (`eur`, `signClass`).
  No cookies, sessions, or DB.
- `internal/controller` — **HTTP layer.** Handlers, auth, sessions, CSRF,
  middleware (`Protect`, `SemiAuth`), TOTP. Orchestrates model + view; keep
  handlers thin.

Rules:
- New SQL → model. New markup/formatting → view. New request logic → controller.
- Don't let `model` import `net/http` or `bcrypt`. Don't let `view` read cookies
  or the DB.
- Handler methods and middleware referenced by `main` must be exported.

## Conventions & hard constraints
- **Single static binary, no cgo.** Keep `CGO_ENABLED=0` buildable. Any new
  dependency must be pure Go. Current deps: `modernc.org/sqlite` (driver name is
  `"sqlite"`, NOT `sqlite3`) and `golang.org/x/crypto/bcrypt`. Prefer stdlib.
- **No JavaScript.** Server-rendered html/template, full-page POST→redirect. CSP
  is `script-src 'none'` (set in `main.go` `securityHeaders`) — inline handlers
  won't run. If you genuinely need JS: self-host it (no CDN; embed under
  `internal/view/static`) AND loosen the CSP.
- **CSS/fonts:** CSP allows only Google Fonts (`fonts.googleapis.com` /
  `fonts.gstatic.com`); everything else is `'self'`.
- **Routing:** Go 1.22 ServeMux. Root is `GET /{$}`; path params via
  `r.PathValue("id")`. Protected pages wrapped in `c.Protect(...)`; 2FA pages in
  `c.SemiAuth(...)`; public routes bare.
- **Templates:** each page file defines only `{{define "title"}}` and
  `{{define "content"}}`; it's parsed together with `layout.html` and executed as
  `"layout.html"`. Register a new page by appending its filename to `pages` in
  `view.go`.
- **embed:** patterns can't use `..`, so embedded assets live under their own
  package dir (`internal/model/schema.sql`, `internal/view/templates`,
  `internal/view/static`).
- **Persistence:** timestamps are RFC3339 TEXT; money is REAL; schema is
  idempotent (`CREATE TABLE IF NOT EXISTS`, applied every boot by `Migrate`).
  SQLite runs WAL + `SetMaxOpenConns(1)` (single writer — don't remove without
  reason).
- **PRG/CSRF:** POST handlers redirect with 303. Every POST form includes
  `<input type="hidden" name="csrf_token" value="{{.CSRF}}">` and the handler
  calls `c.checkCSRF(r)`. CSRF is a double-submit cookie (`lira_csrf`).

## Data model
Tables: households, users, invites, recovery_codes, sessions, accounts,
categories, transactions, holdings, projects, project_items, project_phases,
nav_snapshots, fx_rates, operating_costs, app_meta, pocket_entries, api_tokens.
- **Single shared household ledger.** `households` is one row (a display name);
  `users.role` is `admin`|`member`. Accounts/transactions/holdings are NOT scoped
  by user or household — every member sees and edits the same data. (Per-user
  ownership would be a much larger change; out of scope by design.) **The lone
  exception is pocket money (below), which is private per user.**
- **Pocket money** (`pocket_entries`, `controller/pocket.go`, `/pocket`): a
  **PRIVATE per-user** side-pot — the only user-scoped data in the app. Every
  `Store` method (`ListPocketEntries`/`PocketBalance`/`AddPocketEntry`/
  `DeletePocketEntry`/`PocketByCategory`) takes a `userID` and filters
  `WHERE user_id=?`; the handlers pass `currentUser(r).ID` and there is **no admin
  override**, so one member never sees or touches another's fund. Entries are
  signed (`+` top-up, `−` spend) with an optional category + note; balance = Σ
  amount; the page shows a per-category spend breakdown. It does **not** feed NAV,
  accounts, or any shared/report view — it's a standalone tracker for beers,
  subscriptions, work lunches, etc. `DeletePocketEntry` scopes by `user_id` so a
  guessed id can't cross users. `pocket_entries` is a new table (in `schema.sql`,
  no `ensureColumn`); the `idx_pocket_user` index sits in `schema.sql` too.
- Transactions are editable (`TransactionEdit`/`TransactionUpdate`, `GetTransaction`/
  `UpdateTransaction`) except transfer legs (guarded — edit those by delete+re-add).
  `DeleteTransaction` on a transfer leg deletes **both** legs (shared `transfer_id`),
  so a lone leg can never be left moving a balance/NAV.
- **Categories** are user-managed at `/categories` (`controller/categories.go`):
  add/rename/delete with kind income|expense; delete relies on the transactions FK
  `ON DELETE SET NULL` (leaves rows uncategorized). `CategoryUsage` shows counts.
- Account balance = `opening_balance + Σ(transactions.amount)` (income +, expense −).
- **Transfers** (`Store.CreateTransfer`, `/transfer`): one action writes two
  balanced legs (− from, + to) sharing a `transactions.transfer_id` (added via
  `ensureColumn`); NAV is net-zero and analytics exclude transfer legs. Between
  accounts in **different currencies** `CreateTransfer(from, to, date, sent,
  received, notes)` books each leg in its own currency — the form's "Amount
  received" is required then (ignored for same-currency moves). The
  dashboard shows a **bills-due** banner (active operating costs whose
  `NextDueDate` is within 7 days), via `Controller.billsDue`.
- **Insights** (`/insights`, `controller/insights.go`): `Store.MonthlyTotals`
  (income/expense per `YYYY-MM`) and `Store.SpendByCategory(from,to)` aggregate
  transactions **in EUR** (by account currency, via `Store.eurRate`, like NAV)
  **excluding transfer legs** (`transfer_id IS NULL`); the page shows
  this-month income/spend/net/savings-rate, a category breakdown (CSS bars), and a
  12-month trend table.
- **CSV import/export** (`controller/csv.go`): export handlers (transactions /
  holdings / costs) write `encoding/csv` attachments. Import is a no-JS
  upload→map→commit flow: `ImportPreview` parses the upload (delimiter-aware,
  ragged-tolerant) and renders a column-mapping form with the raw CSV embedded in a
  hidden `<textarea>` (stateless — no temp files); `ImportCommit` re-parses and
  inserts via `CreateTransaction`. Pure tested helpers `parseCSVDate`/`parseCSVAmount`
  (layouts, decimal-comma, debit/credit, accounting negatives, DR/CR, U+2212 and
  trailing minus). `parseCSVAmount` **refuses rather than guesses**: a grouping
  separator not followed by exactly 3 digits (`12,50` with comma-decimals off),
  a separator after the decimal point, stray letters (`1e3`), extra signs — such
  rows are skipped as "Unparseable amount", never imported as a wrong number. 5 MB cap,
  enforced: `ImportPreview` parses the upload before `checkCSRF` and refuses a
  bigger file outright (no silent truncation). Import
  **skips duplicates** by default via `model.TxDedupKey` (date+amount+payee):
  `Store.AccountTxKeys` **counts** copies per key and the import skips only as
  many rows as the account already holds — genuine repeats (two identical coffees)
  import, re-importing the same file adds nothing. Exports pass every cell
  through `csvSafe` (a leading `= + - @`/tab/CR gets an apostrophe so Excel won't
  run it as a formula; plain numbers like `-12.50` stay numeric).
- **Backup** (`/backup`, admin-only, `controller/backup.go`): `Store.Backup` runs
  SQLite `VACUUM INTO` a temp file, streamed as a `.db` download. Settings page button.
- NAV = `Σ(non-spending account balances) + Σ(holding.quantity × holding.price)`
  (accounts flagged `spending` are excluded — see Spending accounts below).
- `invites`: stores the token HASH only; single-use (`used_at`) + expiry.
- **Projects** (`projects` + `project_items`, `controller/projects.go`): a savings
  goal with line-items. **Budget = Σ item estimates × (1 + buffer_pct/100)** (the
  `projects.target_amount` column is vestigial — target is computed). `finance_pct`
  is a *planning* input only (creates no transactions): it splits the budget into
  financed-from-savings (drawn from NAV) and to-save (÷ months to `target_date`).
  Payments draw the **financed share first** (that money exists today): `ToSave`
  is what is still left to save, `min(Target − Financed, max(Remaining, 0))`, so
  the monthly figure and an overdue phase's "due now" shrink as it's paid.
  The savings math lives in the pure, unit-tested `model.ComputePlan`. **Line items
  accept partial payments** (a deposit now, the balance later — no need to split into
  two items): `Store.PayItem(itemID, account, date, amount, categoryID, final)`
  atomically inserts an expense `transactions` row (negative, `project_id` **and**
  `project_item_id` set); the item flips to `purchased` (done) when `final` is set or
  the cumulative payments reach the estimate (a zero-estimate item completes only on
  the explicit flag). **An item's paid total and a project's/phase's "Spent" are
  DERIVED from the ledger** — `-Σ(transactions.amount) WHERE project_item_id=…` (the
  `project_items.amount_paid` column was deliberately NOT kept, so there is no
  denormalized copy to drift when a payment is later deleted or edited; deleting a
  payment self-corrects Spent/Remaining). `ProjectItem.AmountPaid` is a computed
  column in `projectItemCols`; the General group in `projectGroups` sums it. So a
  deposit immediately draws down remaining/cash/burn-down — not just completed items.
  `actual_cost` holds the final total once done; `transaction_id` points at the most
  recent payment. Because those two (and `purchased`) ARE stored, editing or
  deleting a payment row re-derives them in the same transaction
  (`changeTransaction` → `syncItemStatus`: reopen when payments drop below the
  closing total, follow the total while covered, close when an edit reaches the
  estimate); a payment on an already-closed item keeps it closed; `Store.ListItemPayments` lists an item's payment history for the pay
  form. `transactions.project_item_id` is added via `ensureColumn`, with a one-time
  backfill that reconnects legacy single-purchase items (via their `transaction_id`
  link) so their derived spend survives. The ledger stays the single source of truth.
  Projects are household-shared (all members). Every `{itemID}`/`{phaseID}`
  handler resolves the object via `projectItem`/`projectPhase` (and `ItemCreate`'s
  phase via `phaseOfProject`), which require it to belong to the `{id}` project in
  the URL — GETs 404, POSTs change nothing otherwise. `validateProject` also
  rejects finance % outside 0–100, a negative buffer, deposit or APR
  (`ComputePlan` clamps a stored out-of-range %); a negative item estimate is
  ignored like a blank name. Paying the last open item
  auto-completes the project — **except** a loan-financed one whose repayments are
  still to come (`loanRepaying`), since a completed project leaves the budget and
  would silently drop its monthly loan payment.
- **Loan financing** (`projects.finance_method` ∈ `save`|`loan`, plus
  `loan_deposit`/`loan_apr`/`loan_term_months`/`loan_start_date`, all added via
  `ensureColumn`): a
  project chooses its **financing method** on the create/edit form (`ProjectInput`
  carries the fields; `CreateProjectInput`/`UpdateProject` persist them). The pure,
  unit-tested `model.ComputeLoan(budget, deposit, apr, months)` amortizes principal
  = budget − deposit into a monthly payment + total interest ("interest lost") +
  all-in cost (APR 0 → straight split; deposit ≥ budget or term 0 → nothing borrowed).
  **Repayments can start on a future date** (`loan_start_date`, optional):
  `FinanceWithLoan(plan, deposit, apr, term, startDate, now)` gates at month
  granularity — before the start month `MonthlyToSave` is 0 (nothing deducted from
  the budget yet) while `ProjectPlan.LoanStart` + `UpcomingMonthly` carry the date
  and the payment-to-come for the "€X/mo from <date>" labels; mid-term
  `MonthsLeft` counts down; after the last payment both drop to 0 and the pages
  show a "repaid" state. **The deposit is payable in any number of increments, as
  real transactions**: each `Store.PayLoanDeposit` call inserts an expense tagged
  `transactions.deposit_project_id` (and `project_id`, but NOT `project_item_id` —
  so it never counts as item "Spent"). `Project.DepositPaid` is **Σ over the
  tagged rows** (clamped ≥ 0 via `MAX(-SUM(t.amount),0)` in `projectSelect`);
  `DepositPaidDate` is the latest payment's date; `Store.ListDepositPayments`
  feeds the panel's history table. Because the total is ledger-derived over many
  rows — exactly like item payments — deposit payments are **freely editable and
  deletable** (no transfer-leg-style edit guard; a tagged positive "refund" row
  legitimately reduces the total, and the sum self-corrects on any edit/delete).
  The old one-slot `projects.deposit_transaction_id` is **vestigial**: kept only
  for the `Migrate` backfill that tags v1 single-payment deposits with
  `deposit_project_id`. `DepositPay` accepts payments while
  `LoanDeposit − DepositPaid > 0.005` (a sub-half-cent remainder counts as fully
  paid — `ProjectView` clamps `DepositRemaining` with the same epsilon so the
  templates never offer a form the handler would refuse); the panel shows
  planned/paid/remaining with `part-paid`/`paid` tags and the payment history,
  and keeps a method-switched project's history visible with payments disabled.
  The controller helper `stillToDraw`
  (loan: Financed − DepositPaid; save: Financed − the aggregate Spent, which
  includes item payments and any deposit paid before a switch back from loan —
  clamped ≥ 0, so paid money never counts twice) keeps
  `AfterNAV`/`AfterCash`/`ShortCash` and the projects-list "committed" figure
  honest. **A paid deposit also counts toward the project's displayed
  Spent/Remaining** — `projectGroups` adds `DepositPaid` into the aggregate plan
  (any finance method, Remaining clamped ≥ 0) so the hero, cards, progress bars,
  and plan page all draw down when the deposit is paid; per-phase "Spent" and the
  API's `spent` stay item-derived (the deposit is project-level money, not item
  spend); the burn-down includes the **unpaid** deposit remainder as a lump at
  `loan_start_date` (else `target_date`) — skipped (`depositPaidPreLira`) for a
  loan whose start month is already BEFORE the current one (a deposit due this
  month stays a lump) ONLY when no payments are recorded (that implies the deposit
  left pre-Lira; recorded increments prove Lira is tracking it, so the remainder
  stays as a due-now lump). `DepositPay`
  handler at `POST /projects/{id}/deposit`; the pay form lives in the detail
  page's loan panel. Both payment-tag columns are indexed in `Migrate`
  (`idx_tx_project_item`, `idx_tx_deposit_project` — after `ensureColumn`, like
  `idx_pitem_phase`).
  The pure `validateProject` rejects a `loan` method with no term (`ProjectCreate`/
  `ProjectUpdate` re-render the form with the error) — so `finance_method == "loan"`
  always implies a term, keeping `loanFinanced` (which also checks `term > 0`) and
  the templates in agreement. When `finance_method == "loan"` (helper `loanFinanced`),
  `projectGroups` folds the
  loan into the **aggregate plan** via the pure `model.FinanceWithLoan` — so the
  project's monthly commitment becomes the **loan payment** (not target-date saving)
  and `Financed` becomes the **deposit**. This ripples everywhere that reads
  `card.Plan` (the `budgetSummary`/costs "Project funding" line now sums loan
  repayments *and* savings; the projects list, dashboard row, project detail hero,
  and `project_plan` "Loan repayment plan" branch all relabel). Still creates **no
  transactions** — it's a planning commitment against the monthly budget. The detail
  page also keeps the standalone loan **calculator** panel (`LoanUpdate`,
  `POST /projects/{id}/loan`, `UpdateProjectLoan`) which tweaks the loan numbers
  without changing the method (shows an "active" tag when loan is the chosen method);
  it keeps a loan project's stored term when the field is blank/0 (else the
  project would silently turn into a saving plan) and floors deposit/APR at 0.
- **Phases** (`project_phases` + `project_items.phase_id`): a project can be split
  into dated milestones; each item is assigned to a phase (NULL `phase_id` = the
  implicit "General" group, dated by the project's own `target_date`). Each phase
  reuses the pure `model.ComputePlan` for its own budget/saving target; the project
  total is the **concurrent** `model.AggregatePhases` (budgets/financing sum,
  monthly = sum of each unfunded phase's monthly, done phases excluded). The
  controller's `projectGroups` buckets items and folds the aggregate. Deleting a
  phase is `ON DELETE SET NULL` (items survive in General). Items can be reassigned
  between phases/General via `Store.SetItemPhase` (`ItemMove` handler; the handler
  guards that both the item and any target phase belong to the same project). The
  read-only **Saving plan** page (`ProjectPlan` handler, `/projects/{id}/plan`,
  `project_plan.html`) reuses `projectGroups` and the pure `model.BuildSaveSchedule`
  to render a month-by-month schedule (each phase contributes its monthly until its
  date; totals step down) plus per-phase stats. NOTE: the
  `idx_pitem_phase` index is created in `Migrate` *after* `ensureColumn` adds
  `phase_id` (it can't sit in `schema.sql`, which runs first on legacy DBs).
- **Market data** (`internal/market`, `fx_rates` table): the `PriceProvider`/
  `FXProvider` seam is **now implemented** (Twelve Data). A background goroutine in
  `main` (enabled when `LIRA_MARKET_API_KEY` set) refreshes `fx_rates` + holding
  prices each `LIRA_FETCH_INTERVAL_HOURS`, then snapshots. `ComputeNAV` converts
  every account/holding by its currency's `rate_to_eur` (missing → 1.0), so NAV is
  EUR. Failed price lookups preserve the existing price (manual prices survive).
  `RefreshNow`/`/refresh` is the on-demand button. Both go through
  `market.Refresh`, which runs the FX and price updates independently (one
  failing doesn't skip the other) and joins their errors; a failed manual refresh
  redirects to `/holdings?refresh=failed`, which shows a note. `fx_rates` is a new table (no
  `ensureColumn` needed). **Auto-fetch is capped to once per UTC day** via the
  `app_meta` KV table (`Store.AutoFetchedToday`/`MarkAutoFetched`, key
  `market_last_fetch`, marked on success) so restarts don't burn API credits; the
  manual button overrides and also stamps the day. The view `money` func formats non-EUR amounts with the
  currency code; `eur` stays EUR-only. The Investments page shows per-holding
  price/value in the holding's **own** currency, then **per-currency subtotals**
  (native → EUR) and a final EUR grand total, so the EUR total reconciles with the
  native rows instead of appearing under them unlabelled (the `Holdings` handler
  builds `currencyTotal` subtotals; `ShowFX` hides them for an all-EUR portfolio).
  It also shows unrealized gain/loss per holding + total via the pure
  `model.HoldingGain` (value − cost, EUR-converted); `holdings.cost_basis` is the
  **total** amount paid for the position.
  The **Investment forecast** page (`/projection`, `controller/projection.go`,
  linked from Investments) is a stateless calculator: pure `model.BuildProjection`
  (monthly-compounded growth + contributions) → year-by-year table + reused SVG
  `chart` (contributions vs growth, stacked). Inputs come from query params
  pre-filled from current investments / budget available / 25× annual costs (FI
  number); optional inflation shows real value.
- **Operating costs / budget** (`operating_costs` + `households.monthly_income`,
  `controller/costs.go`): recurring expenses (name, category, amount, currency,
  frequency), each normalized to monthly via the pure `model.MonthlyAmount`
  (weekly ×52/12, quarterly ÷3, yearly ÷12) and converted to EUR via `GetFXRates`.
  The `/costs` page groups by a fixed category set and shows the monthly budget:
  **available = income − Σ active costs − Σ active-project monthly savings** (the
  project term reuses `c.projectCards`). `households.monthly_income` (added via
  `ensureColumn`) holds the net income; a dashboard panel mirrors the summary.
  Optional `operating_costs.due_day` (day of month) drives the pure
  `model.NextDueDate` → per-cost next-payment date, an "Upcoming payments" list,
  and the total due in the next 30 days. The costs page also shows a **plan vs
  actual** panel: planned (income / recurring costs / leftover) against this
  month's actuals from `Store.MonthlyTotals` (transfers excluded).
- **Budgets vs actuals** (`operating_costs.category_id` → a transaction category,
  via `ensureColumn`): an operating cost becomes a **budget** when linked to a
  transaction category. `Store.CategorySpend(from,to,spendingOnly)` sums this
  month's expense per category (EUR-converted; expense, non-transfer) and the pure
  `model.BudgetStatus(actual,budget)` returns a percent + state (`ok`/`low` ≥90% /
  `over`). The `Costs` handler shows per-budget actual + bar and a **Budget alerts**
  panel; `c.budgetAlerts` powers dashboard banners (over / running-low). Untracked
  costs stay plain fixed costs.
- **Spending accounts** (`accounts.spending`, via `ensureColumn`): accounts flagged
  as spending accounts **fund operating costs** and are **excluded from NAV**
  (`ComputeNAV` skips them) and therefore from project funding/burn-down (which read
  `ComputeNAV`). Budget actuals are scoped to these accounts when any exist
  (`CountSpendingAccounts > 0`), else fall back to all accounts. The account form has
  a checkbox; the accounts page tags them and shows a "counted in net worth"
  subtotal.
- **NAV history** (`nav_snapshots`, columns `nav_eur`/`cash_eur`/`inv_eur`): a
  background goroutine in `main` calls `Store.EnsureDailySnapshot` (deduped per UTC
  day) on boot + every 6h; `SnapshotNow` records on demand. The **History** page
  (`NavHistory`, `/history`) and a dashboard panel render a **server-rendered inline
  SVG** stacked-area chart via the `view` funcMap func `chart` (operates on a plain
  `view.ChartSeries` — keeps `view` free of `model`). No JS: inline SVG is markup,
  allowed by the CSP. New `nav_snapshots` columns added via `ensureColumn`.
  Chart y-ranges cover every drawn series and 0 (negative cash, a forecast line
  dipping below zero), so nothing renders off the canvas; `eur` shows `€—` for
  NaN/Inf instead of panicking the template, and `eur`/`signClass` treat
  anything under half a cent as zero (no "-€0.00").
- **Dashboard visuals** (`controller/dashboard.go` `dashboardExtras`): the hero
  shows net worth + month-over-month change (current NAV vs the last snapshot before
  this month) + an **asset-allocation donut** (`view` funcMap `donut` over
  `[]view.DonutSeg`); a widget grid (savings rate, portfolio gain %, next bill,
  available); and a budget breakdown with an allocation bar + a net-trend
  **`sparkline`** (funcMap, inherits color via `currentColor`). All server-rendered
  SVG — no JS.
- **Nav** (`layout.html`): a single top `.mainnav` with **all** links inline
  (Dashboard, Accounts, Transactions, Investments, Projects, Costs, Pocket,
  Reports, Settings[admin]); it wraps on narrow screens (`flex-wrap`). No emoji, no
  dropdown. Static assets are served `Cache-Control: no-cache` (embedded files have
  no mod-time) so a CSS change isn't masked by a stale browser cache.
- **NAV forecast / burn-down** (`/forecast`, `controller/forecast.go`,
  `households.monthly_savings` via `ensureColumn`): projects net worth forward from
  today at two saving rates — **planned** (`households.monthly_savings`, set on the
  **Settings** page via `SavingsUpdate`, `POST /settings/savings`, admin-only) and
  **actual** (the recent average net income−expense from `Store.MonthlyTotals`,
  transfers excluded, via `actualMonthlySaving`) — against a **target** line
  (query `target`, default the FI number = 25× annual operating costs). The pure,
  unit-tested `model.BuildNavForecast` (two series, optional monthly-compounded
  return) and `model.MonthsToTarget` (ETA per rate) drive it; a dedicated
  `view.ForecastSeries` + `forecastChart` funcMap func render the two-line SVG with
  a dashed target and a crossover marker (kept separate from the stacked-area
  `navChart`). Read-only — creates no transactions; "linked with transactions"
  means the *actual* line is derived from the ledger. Linked from the History page;
  the Settings savings field hints the budget's free `Available`.
- **Cash burn-down** (`/burndown`, `BurnDown` in `controller/forecast.go`): a
  month-by-month projection of **spendable cash** (not NAV) — starts at current
  liquid cash (`ComputeNAV` cash), adds the planned monthly saving, and subtracts
  each active **save-financed** project's **remaining budget** as a lump at its
  (phase/project) target date (`projectGroups` → `monthsFromNow`). **The planned
  saving means "what the budget leaves free AFTER project commitments"** (the
  Settings hint shows exactly that `Available`), so each save group's own
  `MonthlyToSave` is added back as a per-month inflow (`addSavingStream` → the
  `ins` map) for its `MonthsLeft` months — it covers `ToSave` by the lump month,
  so the lump nets down to the financed share drawn from existing cash. Groups
  whose lump is beyond the horizon (or undated) add neither. Loan-financed
  projects contribute no lump for the borrowed part, BUT: their unpaid deposit
  remainder is a lump at the start date (see Loan financing above), and a loan
  whose repayments **haven't started yet** deducts its monthly payment from the
  projected cash from its start month for the remaining term (`addLoanStream` →
  the `outs` map) — a pre-start loan isn't in today's budget `Available`, so the
  Settings saving figure can't already include it; RUNNING loans are left alone
  (already inside the budget → no double-count). The pure
  `model.BuildBurnDown(start, monthlyIn, ins, outs, events, months)` walks the balance
  (`ins` = per-month project saving; `outs` = per-month recurring outflows, drawn but not spend-dotted; may go
  negative = a "you'd run out" signal); `view.BurnSeries` +
  `burndownChart` draw the single line with spend-dot markers and a red zero
  baseline. Below the chart a **chronological cash ledger** (the pure-ish, unit-
  tested `buildCashLedger`): between spend clusters it folds the GROSS saving as
  a green "Saving × N mo" row (plus a green "Saving for projects" row for that
  window's `ins`) and shows any loan repayments in that window as
  their own explicit red "Loan payments × K mo" row (not silently netted into
  the saving figure), then draws each spend down sequentially; if repayments
  continue past the final spend, a trailing Saving/Loan-payments pair covers the
  rest of the horizon so the last row's Cash equals the chart's end balance
  (previously the timeline just stopped while payments kept running). The running
  balance reconciles with `BuildBurnDown` at every month boundary — pinned by
  `TestBuildCashLedger`. Flags undated / beyond-horizon spends. Read-only.
- **Reports index** (`/reports`, `Reports` handler, `reports.html`): a card grid
  linking the analytical views — History, NAV forecast, Cash burn-down, Insights,
  Investment forecast. The main nav (`layout.html`) groups these under a single
  **Reports** item (History and Insights were moved off the top-level nav).
- `accounts.currency` / `holdings.currency` are stored, but v1 sums everything as
  EUR (FX deferred — see roadmap).
- **Additive migrations:** new columns on existing tables go through
  `Store.ensureColumn` in `Migrate` (idempotent `ALTER TABLE ... ADD COLUMN`),
  since `CREATE TABLE IF NOT EXISTS` won't alter an existing table. New singleton
  rows use `Ensure*` seeders (`EnsureHousehold`, `EnsureAdmin`,
  `EnsureDefaultCategories`) called from `main`.

## JSON API (`/api/v1`, `controller/api.go`)
A read+write JSON API for native clients (e.g. an iOS app) — a **separate surface**
from the no-JS HTML app, so the CSP/server-rendered rules don't apply to it.
- **Auth: per-user bearer token** (`Authorization: Bearer <token>`), via `APIAuth`
  middleware → `Store.UserByAPITokenHash(hashToken(tok))`. Not cookies/CSRF. Tokens
  are minted in the **Settings** page's "API access" section (`renderSettings` lists
  the current user's tokens; create/revoke at `POST /tokens` and
  `POST /tokens/{id}/delete`, `Protect`+scoped by `currentUser`, in
  `controller/tokens.go`; create re-renders Settings showing the secret once). Only
  the SHA-256 hash is stored (`api_tokens`), plaintext shown once (`lira_<rand>`). A
  token authenticates **as its owner**, so per-user data (pocket money) stays scoped.
  The **same token section also appears on the Pocket page** (`renderPocket`), the
  per-user home every member can reach — so non-admin members mint their own tokens
  there (Settings is admin-only). A hidden `from` field (`settings`|`pocket`) tells
  `TokenCreate`/`TokenRevoke` which page to re-render (with the one-time secret) /
  redirect to — a non-admin always gets Pocket, whatever `from` says.
- Endpoints (all `c.APIAuth(...)` in `main.go`): `GET /me`, `/summary`, `/accounts`,
  `/transactions`, `/holdings`, `/projects`, `/costs`, `/pocket`; writes `POST
  /transactions`, `/accounts`, `/holdings`, `/pocket` and `DELETE /transactions/{id}`,
  `/pocket/{id}`. Handlers emit clean JSON via map DTOs (avoids `sql.Null*`
  marshaling); helpers `writeJSON`/`apiError`/`decodeJSON` (1 MB body cap).
  Write dates must be `YYYY-MM-DD` (`isDate`, else 422) — reports group and
  range-filter on that text form.
- Deferred (easy to add): edits/`PUT`, projects/costs writes, transfers, pagination.
  Served over TLS (Cloudflare/Caddy) — token + HTTPS is the whole model.

## Auth & security model
- **Invite-only.** Public `/register` works ONLY to create the first account =
  **admin**; afterwards `registrationOpen()` is false and the link is hidden.
  Members join via admin-issued one-time `/invite/{token}` links (minted in
  `settings.go`, consumed by `InviteAccept`). `LIRA_MAX_USERS` caps total accounts.
- **Roles & admin gate:** admin-only routes use `c.Protect(c.AdminOnly(...))`.
  `EnsureAdmin` promotes the earliest user if none is admin (pre-roles DBs).
- **Invites:** SHA-256 hash stored (plaintext shown once); single-use + 7-day
  expiry (`inviteTTL`); absolute link built from `cfg.BaseURL` else request host.
- TOTP 2FA is mandatory: after register/login, `Protect` forces `/2fa/setup`
  (enrol) then per-login `/2fa/verify`. RFC 6238, ±1 step, constant-time compare
  (hand-rolled in `controller/totp.go`). No QR — setup shows the key + otpauth URI
  for manual entry.
- Recovery codes: 10, shown once, bcrypt-hashed, single-use.
- Passwords: SHA-256 pre-hash → bcrypt cost 12 (avoids the 72-byte limit).
- Sessions: server-side (`sessions` table), httpOnly + SameSite=Lax cookie
  (`lira_session`), Secure per `LIRA_SECURE_COOKIES`.
- Login refusals (unknown user, wrong password, **locked**) all show one message
  and pay the same bcrypt cost (`dummyHash`), so the lockout can't confirm that a
  username exists.
- Login lockout: 5 failures → 15 minutes. Every guess first **claims** an attempt
  with `Store.ClaimLoginAttempt` — one conditional `UPDATE … RETURNING` that
  refuses while locked, counts the guess as a failure up front, and sets the
  lockout on the claim that reaches the limit — so parallel requests can't all
  pass a stale "not locked" read; a correct guess undoes it. The **same lockout
  guards `/2fa/verify`** (reset on success) so a stolen password can't
  brute-force the second factor. For a TOTP user a correct password only gives
  back its own claim (`RefundLoginAttempt`), never resets the counter (only a
  passed 2FA does), else re-logging in after every 4 wrong codes would bypass it.
- **One-time things are consumed atomically** — act only when the conditional
  `UPDATE` changed a row: TOTP steps (`ConsumeTOTPStep`, `WHERE totp_last_step<?`),
  recovery codes (`UseRecoveryCode`, `WHERE used=0`), invites (`AcceptInvite`
  claims the invite and inserts the user in one transaction), and the user cap
  (`CreateUserCapped`, `INSERT … SELECT … WHERE COUNT(*) < cap`; `/register`
  uses cap 1). Pinned by the parallel tests in `controller/auth_lockout_test.go`.

## Config (env, `LIRA_*`)
`ADDR` (:8080), `DB` (./lira.db), `SECURE_COOKIES` (true), `MAX_USERS` (2),
`SESSION_TTL_HOURS` (168), `ISSUER` (Lira), `BASE_URL` (empty → derive from
request; set to the public origin for correct invite links behind a proxy),
`MARKET_API_KEY` (empty → price/FX fetch off), `FETCH_INTERVAL_HOURS` (24).
See `config.example.env`. Request bodies are capped in `main.go` `limitBody`
(1 MB; `controller.ImportBodyLimit` on `/transactions/import/*`).

## Deploy
Static binary + `deploy/lira.service` (hardened systemd unit). Plain HTTP behind
Caddy or a Cloudflare tunnel for TLS; keep `SECURE_COOKIES=true`. Full steps in
README.

## Roadmap — original four all shipped
1. ~~**Projects** — savings-goal UI.~~ **DONE** (+ phases, item moves, saving plan).
2. ~~**Charts / NAV history**~~ **DONE** — daily snapshots + server-rendered SVG.
3. ~~**EUR FX conversion**~~ **DONE** — `fx_rates` + FX-aware `ComputeNAV`.
4. ~~**Auto-fetch prices**~~ **DONE** — `internal/market` (Twelve Data) behind the
   `PriceProvider`/`FXProvider` seam; background refresh in `main`.

Possible next ideas (not committed): per-holding manual-price opt-out, provider
selection via config, multi-base currency.

## Adding things — quick recipes
- **New page:** add `internal/view/templates/<name>.html` (with the two define
  blocks) → append `<name>.html` to `pages` in `view.go` → add a `Controller`
  handler → register the route in `main.go` (wrap with `Protect` / `SemiAuth` as
  needed) → include the CSRF hidden field in any form.
- **New entity/table:** add the type + `Store` methods in `model.go` → add
  `CREATE TABLE IF NOT EXISTS` to `schema.sql` → wire controller + view.
- **New dependency:** must be pure Go (no cgo). Run `go mod tidy`, then confirm
  `make build` still yields a static binary.

## Known minor issues (intentionally deferred)
- List handlers swallow DB errors and render empty (acceptable for 2 users). The
  pages that compute net worth (dashboard, projects, project plan/detail, forecast,
  burn-down, projection, history) instead **500 on a `ComputeNAV` error** rather than
  rendering fabricated zeros — a deliberate choice, since wrong financial numbers are
  worse than a visible failure.
- 2FA pages still show the app nav (links bounce back) — cosmetic.
- A fully-paid project still shows its contingency **buffer as "remaining"** (the
  buffer is unspent runway, never drawn down); cosmetic, predates partial payments.

## Tests
`go test ./...`. `internal/view` verifies every template parses + executes with
realistic data; `internal/controller` verifies TOTP against RFC 6238 vectors.
Add tests alongside the package you touch.
