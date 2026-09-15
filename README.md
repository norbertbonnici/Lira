# Lira

A small, self-hosted personal-finance app for tracking net worth, accounts,
transactions and investments — built to replace Maybe for a two-person household
and to run comfortably on a ZimaBoard.

It compiles to a **single static binary** (no runtime, no cgo), reads/writes a
local **SQLite** file, and ships its templates + CSS embedded in the binary.

## What's in this first cut

- **Invite-only households**: a bootstrapped admin account, then members join by one-time invite link (no open registration)
- Username + password login with **mandatory TOTP 2FA** (authenticator apps) and one-time recovery codes
- **Accounts** (savings / checking / cash) with balances derived from transactions
- **Transactions** (income / expense, categories, payee, notes) + **transfers** between accounts
- **CSV import/export** — flexible bank-statement import (map columns, dates, debit/credit) and CSV export
- **Investments** (stocks / ETFs; prices auto-fetched or manual) with unrealized
  gain/loss vs cost basis
- **Multi-currency**: non-EUR accounts/holdings converted to EUR for net worth via auto-fetched FX rates
- **Projects** — savings-goal budgets broken into line-items, with a financing
  plan ("how much to save per month" and "how much to draw from savings"), plus an
  optional **loan calculator** (deposit + APR + term → monthly payment and total
  interest). Line-items take **partial payments** — pay a deposit now and the
  balance later, no need to split an item in two
- **Operating costs & budgets** — recurring bills/subscriptions normalized to a
  monthly budget (income − costs − project savings = what's free); link a **weekly or
  monthly** cost to a transaction category to track **actual vs budget** this month,
  with **alerts** at ≥90% and over budget (e.g. Groceries €400 budget, €505 spent →
  over). Day-to-day
  spending can come from dedicated **spending accounts** that are excluded from net
  worth and project funding
- **Pocket money** — a *private* per-user fund (top-ups & spends with categories) for beers, subscriptions, work lunches; only you can see yours, separate from the shared household ledger
- **Dashboard** — net worth with month-over-month change, an asset-allocation donut, at-a-glance widgets (savings rate, portfolio gain, next bill, available), a budget breakdown with an allocation bar, plus active projects and recent activity. The analytical views (history, forecasts, insights) are grouped under a single **Reports** link to keep the top nav tidy. All charts are server-rendered inline SVG — no JavaScript
- **Net worth history** — daily snapshots with a server-rendered SVG chart (cash vs investments over time)
- **Net worth forecast** — project NAV forward at your planned monthly saving (set in Settings) vs your actual saving rate from the ledger, against a target line, with the projected date you hit it
- **Cash burn-down** — month-by-month spendable cash: your saving builds it up and each project's budget draws it down on its target date (e.g. €100k, save €2k/mo, a €80k purchase in 2 months → see the dip and recovery), with a warning if you'd run dry
- **Insights** — spending by category, a 12-month income/expense trend, and your savings rate
- **Investment forecast** — project ETF/portfolio growth over coming years (contributions + compound return), with a FIRE target and inflation

That rounds out the original roadmap. See **Market data** below for the
auto-fetched prices + FX.

### Projects

A project (e.g. *Kitchen remodel*) is a budget with a target purchase date,
broken into line-items (table, chairs, oven). The budget is the sum of item
estimates, optionally padded by a **contingency buffer %**. You then pick a
**finance-from-savings %**, and Lira splits the budget into:

- **Financed from savings** — drawn from your existing net worth (shown against
  both full NAV and liquid cash, so you can see what it leaves you).
- **To save** — spread across the months until the target date → "save €X/month".

**Paying for an item** records a real expense transaction tagged to the project,
so your account balance, net worth, and budget-vs-actual all stay in sync. You can
**pay in parts** — a deposit now, the balance later — without splitting the item in
two: each line-item shows how much you've paid and what's left, and a deposit
immediately draws down the project's remaining budget, cash, and burn-down (not just
completed items). An item completes automatically once it's fully paid, or tick
**"final payment"** to close it out if the final price came in under your estimate.
Paid amounts are derived from the ledger, so deleting or editing a payment just
re-syncs the numbers — there's no separate copy to drift. A project auto-completes
once every item is fully paid.

**Phases** let you split a project into dated milestones (e.g. Demolition by Mar,
Cabinets by Jun, Appliances by Sep). Each line-item is assigned to a phase, so a
phase has its own budget (its items) and its own saving target to its date. The
project's monthly figure is the **sum** across phases (concurrent), and each
phase shows its own. Items left unassigned sit in a "General" group dated by the
project's overall target; deleting a phase drops its items back there. You can
**move any item between phases (or back to General)** from a per-row picker on
the project page. Projects with no phases behave exactly as before.

Each project has a **Saving plan** page (linked from the project) showing summary
stats, a per-phase breakdown, and a **month-by-month schedule** — the combined
amount to save each month, which steps down as nearer phases reach their dates
(rendered as a table plus a CSS bar chart, no JS).

Each project picks a **financing method** when you create or edit it:

- **Save up** (default) — the target-date saving plan above.
- **Loan** — enter an upfront **deposit**, an **APR**, and a **term in months**.
  Lira amortizes the borrowed amount (budget − deposit) into a fixed **monthly
  payment**, the **total interest** you'd pay ("interest lost"), and the **all-in
  cost** versus paying cash. A loan-financed project's monthly figure becomes the
  **loan repayment** (not a per-month saving), only the **deposit** is drawn from
  savings, and that repayment is what shows up in your **monthly budget** (income −
  costs − project funding). Optionally set a **repayments-start date**: until that
  month the repayment is *not* deducted from your budget (pages label it "from
  <date>"), mid-term the months remaining count down, and a finished loan stops
  deducting entirely. The borrowed part records no transactions — it's a planning
  commitment — but the **deposit is payable for real, in one go or in
  increments**: the project page's Deposit panel records each payment as an actual
  expense tagged to the project (with a running planned / paid / remaining
  breakdown and a payment history), so your balance and net worth reflect the cash
  handed over and the projections stop counting it as still-to-draw. Deposit
  payments are ordinary transactions — **edit or delete them freely** and the
  totals follow the ledger automatically, and the paid amount **draws down the
  project's Spent/Remaining and progress bar** like any money into the project.
  The cash burn-down shows the **unpaid remainder** of a deposit as a lump at the
  loan start date, and a loan that hasn't started yet **deducts its monthly
  repayment from the projected saving** from its start month (running loans are
  already part of your monthly budget, so they aren't double-counted).

The project page keeps a standalone **loan calculator** too, so you can model
*save up* vs *finance it* on a project even before committing to the loan method.

> Heads up: this was written without a Go toolchain on hand to compile against,
> so treat the first `go build` as the real check. If anything trips the
> compiler, send me the error — it'll be a quick fix.

## Build

Requires Go 1.24+ (raised by `golang.org/x/crypto` v0.45; the app's net/http routing itself needs ≥1.22).

```bash
make tidy      # resolves dependencies, writes go.sum  (run once, needs internet)
make build     # -> ./lira (static, CGO_ENABLED=0)
```

Cross-compile for the ZimaBoard from any machine (it's x86-64 Linux):

```bash
make linux     # -> ./lira-linux-amd64
```

## Run locally

```bash
# plain HTTP on localhost for a quick look:
LIRA_SECURE_COOKIES=false ./lira
# open http://localhost:8080
```

`SECURE_COOKIES=false` is only for local HTTP testing — leave it `true` once
there's TLS in front (see below), otherwise the browser won't return the
session cookie.

## First-run flow

1. Open the app → **Register**. On a fresh database this creates the **admin**
   account (the only time the public register page works) and logs you in.
2. You're taken to **2FA setup**: add the key to your authenticator, confirm a
   code, then save the recovery codes shown once.
3. Once the admin exists, public registration **closes** and the link disappears.
   To add another member, go to **Settings → Invite a member**, generate a one-time
   invite link, and send it to them.
4. The invited member opens the link, picks a username + password, and goes through
   2FA setup. Their account is a regular **member**.
5. Both logins share the same household ledger (accounts, transactions,
   investments). `LIRA_MAX_USERS` (default 2) caps the total number of accounts.

In **Settings** (admin only) you can also **name the household**, revoke pending
invites, and **download a full database backup** (one SQLite file). Invite links
are single-use and expire after 7 days.

### Net worth history

Lira records a **NAV snapshot once per day** (a background goroutine, deduped per
day, plus one on boot) capturing total net worth split into cash and investments.
The **History** page (and a dashboard panel) renders the trend as a
**server-rendered inline SVG** stacked-area chart — cash on the bottom,
investments on top, the top edge being total NAV — with no JavaScript (the strict
CSP forbids it). A **Record snapshot** button captures an extra point on demand.

### CSV import / export

**Export** transactions, holdings, or operating costs as CSV (download buttons on
each page). **Import** bank statements into the ledger: upload any CSV, then a
no-JS mapping step lets you match the file's columns to Lira's fields, pick the
**delimiter** (comma/semicolon/tab), **date format**, decimal style (comma or
dot), and either a single signed amount column or separate **debit/credit**
columns. Unparseable rows are skipped and reported; the rest import into the
account you choose. The CSV is carried between steps in the form itself — no
server-side temp files. **Duplicate rows are skipped** by default (same
date + amount + payee already in that account), so re-importing a statement is safe.

### Operating costs (monthly budget)

The **Costs** page is a register of recurring expenses — electricity, water,
internet, phone, groceries, subscriptions (Netflix, iCloud+…), insurance, fuel —
each with an amount and a **frequency** (weekly / monthly / quarterly / yearly),
normalized to a per-month figure (and converted to EUR if in another currency).
Costs are grouped by category with subtotals and a monthly + annual grand total;
each can be **paused** (excluded from totals) without deleting it. Give a cost a
**due day** (day of month) and Lira shows its **next payment date** plus an
**Upcoming payments** list and the total **due in the next 30 days**.

Enter your **monthly net income** and Lira shows the budget that matters:

> **available = income − operating costs − project savings**

where *project savings* is the combined monthly target across your active
projects. The same summary appears as a panel on the dashboard. A **plan vs
actual** panel then compares the plan to this month's real ledger (income,
spending, and what's left) so you can see how the month is tracking.

### Market data (auto prices + EUR FX)

Set `LIRA_MARKET_API_KEY` to a free [Twelve Data](https://twelvedata.com) key and
Lira will, on boot and every `LIRA_FETCH_INTERVAL_HOURS` (default 24):

1. refresh **EUR FX rates** for every non-EUR currency in use (`fx_rates` table), and
2. refresh **holding prices** for every holding whose `symbol` the provider knows.

Net worth is then computed in **EUR** (each account/holding converted by its
currency's rate). The automatic fetch runs **at most once per UTC day** (a successful pull is
recorded, so restarts/redeploys won't re-fetch and burn API credits); the
manual **Refresh prices** button is the override. A failed price lookup leaves
that holding's existing price untouched, so manual prices survive for symbols the
provider doesn't cover, and the app degrades gracefully offline. There's a **Refresh prices** button on the
Investments page for an on-demand fetch. Use the provider's symbol (e.g. `AAPL`)
for auto-pricing. **No key → the feature is dormant** and every currency is treated
as EUR until rates exist.

The fetcher sits behind `PriceProvider` / `FXProvider` interfaces in
`internal/market`, so swapping Twelve Data for another source is a small change.

## JSON API (for a phone app or scripts)

A read+write JSON API lives under **`/api/v1`** — a separate surface from the
web UI (no cookies/CSRF, no HTML). It's how a native app (e.g. iOS) reads and
writes your data.

**Auth:** mint a personal token on the **API tokens** page (top-right link, or
`/tokens`). It's shown once; only its hash is stored. Send it as a bearer token:

```bash
curl -H "Authorization: Bearer lira_xxx" https://lira.example.com/api/v1/summary
```

A token acts as **you**, so your private pocket money is only visible to your own
tokens. Run it over HTTPS (you already do, behind Cloudflare/Caddy).

**Endpoints:**

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/api/v1/me` | user + household |
| GET | `/api/v1/summary` | net worth, cash, investments, budget |
| GET / POST | `/api/v1/accounts` | list / create |
| GET / POST | `/api/v1/transactions` | list (`?limit=`) / create |
| DELETE | `/api/v1/transactions/{id}` | delete |
| GET / POST | `/api/v1/holdings` | list / create |
| GET | `/api/v1/projects` | list |
| GET | `/api/v1/costs` | list (with `monthlyEUR`) |
| GET / POST | `/api/v1/pocket` | your private fund: balance + entries / add |
| DELETE | `/api/v1/pocket/{id}` | delete your entry |

Create example:

```bash
curl -H "Authorization: Bearer lira_xxx" -H "Content-Type: application/json" \
  -d '{"accountId":1,"date":"2026-06-10","amount":-42.50,"payee":"Coffee"}' \
  https://lira.example.com/api/v1/transactions
```

## Deploy on the ZimaBoard

```bash
sudo useradd --system --home /opt/lira --shell /usr/sbin/nologin lira
sudo mkdir -p /opt/lira
sudo cp lira-linux-amd64 /opt/lira/lira
sudo cp config.example.env   /opt/lira/lira.env   # then edit
sudo cp deploy/lira.service /etc/systemd/system/
sudo chown -R lira:lira /opt/lira

sudo systemctl daemon-reload
sudo systemctl enable --now lira
journalctl -u lira -f
```

### Updating (redeploy a new build)

From your dev machine, one command builds the Linux binary and rolls it out:

```bash
make deploy LIRA_HOST=user@your-server
```

That cross-compiles (`make linux`), copies the binary and `deploy/deploy.sh` to
the host, then runs the script under `sudo`. The script (`deploy/deploy.sh`)
stops the service, backs up the current binary, installs the new one
(`/opt/lira/lira`, owned by `lira:lira`), restarts, and **auto-rolls-back** to the
previous binary if the new one fails to come up. It refuses to deploy anything
that isn't a Linux ELF (so a macOS `make build` artefact can't slip through).

You can also run it directly on the host:

```bash
sudo ./deploy.sh /path/to/lira-linux-amd64
# optional HTTP probe after restart:
HEALTH_URL=http://localhost:8080/login sudo ./deploy.sh ./lira-linux-amd64
```

### TLS / exposure

The app speaks plain HTTP and expects something in front for TLS. Two easy options:

- **Caddy** on the same VM for automatic local HTTPS, reverse-proxying to `127.0.0.1:8080`.
- A **Cloudflare tunnel** straight to `127.0.0.1:8080` (and Cloudflare Access in
  front if you want an extra auth gate at the edge).

Keep `LIRA_SECURE_COOKIES=true` in both cases.

## Configuration

All via environment (see `config.example.env`):

| Variable | Default | Notes |
|---|---|---|
| `LIRA_ADDR` | `:8080` | Listen address |
| `LIRA_DB` | `./lira.db` | SQLite file (WAL mode) |
| `LIRA_SECURE_COOKIES` | `true` | `false` only for local HTTP |
| `LIRA_MAX_USERS` | `2` | Total account cap (admin + invited members) |
| `LIRA_SESSION_TTL_HOURS` | `168` | Session lifetime |
| `LIRA_ISSUER` | `Lira` | Authenticator label |
| `LIRA_BASE_URL` | _(empty)_ | Public origin for invite links; falls back to request host |
| `LIRA_MARKET_API_KEY` | _(empty)_ | Twelve Data key; enables auto price + FX fetch (empty = off) |
| `LIRA_FETCH_INTERVAL_HOURS` | `24` | How often to refresh prices + FX |

## Backups

It's one SQLite file. Easiest safe copy while running:

```bash
sqlite3 /opt/lira/lira.db ".backup '/path/backup.db'"
```

(Or drop in Litestream against the same file for continuous replication.)

## Security notes

- Passwords: SHA-256 pre-hash (avoids bcrypt's 72-byte truncation) then bcrypt (cost 12).
- TOTP: RFC 6238, ±1 step window, constant-time comparison; recovery codes are bcrypt-hashed and single-use.
- Sessions: server-side, random tokens, httpOnly + SameSite=Lax cookies (Secure when configured), expiry enforced.
- CSRF: double-submit cookie token validated on every state-changing POST.
- Login throttling: 5 failures → 15-minute lockout — the same throttle guards the **2FA verify** step, so a stolen password can't brute-force the TOTP code or recovery codes.
- Response headers: strict CSP (`script-src 'none'`), `nosniff`, `X-Frame-Options: DENY`.

## Architecture (MVC)

The code is split into layers with a one-directional dependency flow
(`main → controller → {model, view}`); the model and view know nothing about
each other or about HTTP.

```
main.go                       composition root: config, DB, router, server, middleware
internal/
  config/   config.go         environment-driven configuration
  model/    model.go          MODEL — domain types + SQLite store (data only)
            schema.sql         embedded schema (applied idempotently)
  view/     view.go           VIEW — template parsing/render + display funcs
            templates/*.html   html/template views
            static/style.css   embedded stylesheet
  controller/
            controller.go      CONTROLLER — render wrapper + app handlers
            auth.go            passwords, sessions, CSRF, middleware (Protect/SemiAuth/AdminOnly)
            auth_handlers.go   register (admin bootstrap), invite accept, login, logout, 2FA
            settings.go        admin settings: household name + invite links
            projects.go        savings-goal projects + line-items + payments (deposits/balance)
            costs.go           operating costs + monthly budget summary
            insights.go        spending analytics (categories, trend, savings rate)
            csv.go             CSV export + flexible bank-statement import
            history.go         NAV history + manual snapshot/refresh handlers
            totp.go            RFC 6238 TOTP + recovery codes (stdlib only)
  market/   market.go         MARKET — Twelve Data price/FX fetcher behind interfaces
deploy/     lira.service       systemd unit
```

- **Model** (`internal/model`): all SQL lives here. Pure persistence — no HTTP,
  templating, or password hashing. It only generates opaque session IDs.
- **View** (`internal/view`): owns templates, static assets, and formatting
  (`eur`, sign classes). `Render` receives data and writes HTML; it touches no
  cookies, sessions, or DB.
- **Controller** (`internal/controller`): request handling, authentication,
  sessions, CSRF; orchestrates model + view. Handlers are thin.

