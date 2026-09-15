-- Lira schema. All statements are idempotent and run on every startup.
-- Timestamps are stored as RFC3339 TEXT. Money is stored as REAL.

CREATE TABLE IF NOT EXISTS households (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT    NOT NULL DEFAULT 'Household',
    monthly_income REAL    NOT NULL DEFAULT 0,   -- net household income (EUR), for the budget view
    monthly_savings REAL   NOT NULL DEFAULT 0,   -- planned monthly saving (EUR), for the NAV forecast
    created_at     TEXT    NOT NULL
);

-- Recurring operating costs (bills, subscriptions, …), normalized to monthly.
CREATE TABLE IF NOT EXISTS operating_costs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    category   TEXT    NOT NULL DEFAULT 'Other',
    amount     REAL    NOT NULL DEFAULT 0,
    currency   TEXT    NOT NULL DEFAULT 'EUR',
    frequency  TEXT    NOT NULL DEFAULT 'monthly',  -- weekly | monthly | quarterly | yearly
    due_day    INTEGER NOT NULL DEFAULT 0,           -- day of month (1-31); 0 = unset
    category_id INTEGER REFERENCES categories(id) ON DELETE SET NULL, -- budget tracks this txn category's actuals
    active     INTEGER NOT NULL DEFAULT 1,
    created_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL DEFAULT 'member',  -- 'admin' | 'member'
    totp_secret    TEXT    NOT NULL DEFAULT '',
    totp_enabled   INTEGER NOT NULL DEFAULT 0,
    totp_last_step INTEGER NOT NULL DEFAULT 0,   -- last consumed TOTP step (replay guard)
    failed_logins INTEGER NOT NULL DEFAULT 0,
    locked_until  TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL
);

-- Invite-only registration: an admin mints a one-time tokenised link. Only the
-- token hash is stored; the plaintext is shown once at creation.
CREATE TABLE IF NOT EXISTS invites (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT    NOT NULL UNIQUE,
    label      TEXT    NOT NULL DEFAULT '',
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at TEXT    NOT NULL,
    expires_at TEXT    NOT NULL,
    used_at    TEXT    NOT NULL DEFAULT ''      -- '' = unused (single-use)
);

CREATE TABLE IF NOT EXISTS recovery_codes (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash TEXT    NOT NULL,
    used      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    mfa_passed INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL,
    expires_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS accounts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL,
    type            TEXT    NOT NULL DEFAULT 'savings',
    currency        TEXT    NOT NULL DEFAULT 'EUR',
    opening_balance REAL    NOT NULL DEFAULT 0,
    archived        INTEGER NOT NULL DEFAULT 0,
    spending        INTEGER NOT NULL DEFAULT 0,   -- 1 = funds operating costs; excluded from NAV/projects
    created_at      TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS categories (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT    NOT NULL,
    kind TEXT    NOT NULL DEFAULT 'expense'   -- 'income' | 'expense'
);

CREATE TABLE IF NOT EXISTS projects (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL,
    target_amount REAL    NOT NULL DEFAULT 0,        -- vestigial; target derived from items
    target_date   TEXT    NOT NULL DEFAULT '',
    account_id    INTEGER REFERENCES accounts(id) ON DELETE SET NULL,  -- default purchase account
    status        TEXT    NOT NULL DEFAULT 'saving', -- planning | saving | completed | archived
    finance_pct   REAL    NOT NULL DEFAULT 0,        -- % of budget covered from existing savings
    buffer_pct    REAL    NOT NULL DEFAULT 0,        -- contingency padding on item estimates
    finance_method TEXT   NOT NULL DEFAULT 'save',   -- 'save' (target-date saving) | 'loan' (amortized)
    loan_deposit  REAL    NOT NULL DEFAULT 0,        -- upfront deposit; principal = budget − deposit
    loan_apr      REAL    NOT NULL DEFAULT 0,        -- annual interest rate (%) for the modelled loan
    loan_term_months INTEGER NOT NULL DEFAULT 0,     -- repayment term in months; 0 = no loan modelled
    loan_start_date TEXT  NOT NULL DEFAULT '',       -- YYYY-MM-DD repayments begin; '' = immediately
    deposit_transaction_id INTEGER REFERENCES transactions(id) ON DELETE SET NULL,  -- vestigial (v1 single-payment deposits); superseded by transactions.deposit_project_id
    created_at    TEXT    NOT NULL
);

-- Line-items make up a project's budget; purchasing one records a real expense
-- transaction (transaction_id) so spent-vs-budget and net worth stay truthful.
-- A project can be split into phases (dated milestones); each line-item may
-- belong to a phase. Items with no phase fall into the implicit "General" group.
CREATE TABLE IF NOT EXISTS project_phases (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    target_date TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS project_items (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id     INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    phase_id       INTEGER REFERENCES project_phases(id) ON DELETE SET NULL,  -- NULL = General
    name           TEXT    NOT NULL,
    estimated_cost REAL    NOT NULL DEFAULT 0,
    actual_cost    REAL,                              -- nullable; final total, set when fully paid
    essential      INTEGER NOT NULL DEFAULT 1,        -- 1 must-have, 0 nice-to-have
    purchased      INTEGER NOT NULL DEFAULT 0,        -- 1 = fully paid / done
    transaction_id INTEGER REFERENCES transactions(id) ON DELETE SET NULL,  -- most recent payment
    created_at     TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS transactions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id  INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    date        TEXT    NOT NULL,                 -- YYYY-MM-DD
    amount      REAL    NOT NULL,                 -- signed: + income, - expense
    category_id INTEGER REFERENCES categories(id) ON DELETE SET NULL,
    payee       TEXT    NOT NULL DEFAULT '',
    notes       TEXT    NOT NULL DEFAULT '',
    project_id  INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    project_item_id INTEGER REFERENCES project_items(id) ON DELETE SET NULL,  -- set on project-item payments
    deposit_project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,    -- set on loan-deposit payments (may be several per project)
    transfer_id INTEGER,                          -- groups the two legs of a transfer (not real spend/income)
    created_at  TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS holdings (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    symbol     TEXT    NOT NULL,
    name       TEXT    NOT NULL DEFAULT '',
    kind       TEXT    NOT NULL DEFAULT 'stock',  -- 'stock' | 'etf'
    quantity   REAL    NOT NULL DEFAULT 0,
    price      REAL    NOT NULL DEFAULT 0,         -- current price (manual for now)
    currency   TEXT    NOT NULL DEFAULT 'EUR',
    cost_basis REAL,                               -- nullable
    updated_at TEXT    NOT NULL,
    created_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS nav_snapshots (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    taken_at TEXT NOT NULL,
    nav_eur  REAL NOT NULL,                 -- total net worth
    cash_eur REAL NOT NULL DEFAULT 0,       -- Σ account balances
    inv_eur  REAL NOT NULL DEFAULT 0        -- Σ holding values
);

-- Currency → EUR multipliers, refreshed by the background fetcher.
CREATE TABLE IF NOT EXISTS fx_rates (
    currency    TEXT PRIMARY KEY,           -- e.g. 'USD'
    rate_to_eur REAL NOT NULL,              -- 1 unit of currency in EUR
    updated_at  TEXT NOT NULL
);

-- Small key/value store for app state (e.g. last market-data fetch time).
CREATE TABLE IF NOT EXISTS app_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Personal API tokens for the JSON API (one user can mint several). Only the
-- SHA-256 hash is stored; the plaintext is shown once. Per-user, so a token
-- authenticates as its owner (private pocket money stays scoped).
CREATE TABLE IF NOT EXISTS api_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL DEFAULT '',
    token_hash   TEXT    NOT NULL UNIQUE,
    created_at   TEXT    NOT NULL,
    last_used_at TEXT    NOT NULL DEFAULT ''
);

-- Per-user PRIVATE pocket money. Unlike the rest of the (shared) ledger, these
-- rows are scoped by user_id and only ever read/written for the logged-in user.
-- Not part of NAV/accounts. Amount: + top-up, − spend.
CREATE TABLE IF NOT EXISTS pocket_entries (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    date       TEXT    NOT NULL,
    amount     REAL    NOT NULL,
    category   TEXT    NOT NULL DEFAULT '',
    note       TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tx_account  ON transactions(account_id);
CREATE INDEX IF NOT EXISTS idx_tx_date     ON transactions(date);
CREATE INDEX IF NOT EXISTS idx_tx_project  ON transactions(project_id);
CREATE INDEX IF NOT EXISTS idx_sess_user   ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_pitem_project ON project_items(project_id);
CREATE INDEX IF NOT EXISTS idx_phase_project ON project_phases(project_id);
CREATE INDEX IF NOT EXISTS idx_pocket_user  ON pocket_entries(user_id);
CREATE INDEX IF NOT EXISTS idx_apitoken_user ON api_tokens(user_id);
-- idx_pitem_phase is created in Migrate(), AFTER ensureColumn adds phase_id to
-- pre-existing project_items tables (it can't be indexed before it exists).
