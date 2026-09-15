# Repository Guidelines

## Project Structure & Module Organization

Lira is a self-hosted personal-finance application built as one static Go binary with SQLite and embedded HTML/CSS.

- `main.go`: configuration, database initialization, routing, and server middleware.
- `internal/controller/`: HTTP handlers, authentication, CSRF, and financial calculations.
- `internal/model/`: domain types, SQL persistence, and embedded `schema.sql`.
- `internal/view/`: rendering helpers, `templates/*.html`, and `static/style.css`.
- `internal/config/`: environment configuration; `internal/market/`: price and FX providers.
- `deploy/`: deployment script and systemd unit. Tests live beside source as `*_test.go`.

Keep dependencies flowing `main → controller → {model, view}`. Put SQL in model, presentation in view, and request handling in controller. Read `CLAUDE.md` for detailed domain constraints.

## Build, Test, and Development Commands

Use Go 1.25+, following `go.mod`.

- `make tidy`: resolve dependencies and update module files; requires network access.
- `LIRA_SECURE_COOKIES=false make run`: run locally over HTTP at `http://localhost:8080`.
- `make build`: produce the static `./lira` binary with CGO disabled.
- `make linux`: cross-compile `./lira-linux-amd64` for the ZimaBoard.
- `go test ./...`: run all tests.
- `go vet ./...`: run Go static checks.

## Coding Style & Naming Conventions

Format Go changes with `gofmt`; use standard tab indentation, exported `PascalCase` names, and unexported `camelCase` names. Match existing two-space HTML indentation and descriptive snake_case template filenames.

Prefer the standard library and pure-Go dependencies. Preserve the server-rendered UI: the CSP disables JavaScript. New pages define `title` and `content` templates and must be registered in `internal/view/view.go`. Web POST forms require CSRF protection; successful submissions use 303 redirects.

## Testing Guidelines

Use Go's standard `testing` package, `TestXxx` functions, and table-driven cases where useful. Database tests use temporary SQLite stores. Cover changed financial calculations, persistence behavior, authentication boundaries, and template rendering. Run focused tests with `go test ./internal/controller -run TestStillToDraw`, then the full suite. No numeric coverage threshold is configured.

## Commit & Pull Request Guidelines

History uses concise, descriptive subjects, sometimes prefixed with a topic such as `docs:` or `deploy:`; Conventional Commits are not consistently used. Describe the concrete behavior changed.

PRs should explain what changed and why, include validation commands/results, and note schema or configuration changes. Link relevant issues and include screenshots for UI changes. Follow `PR_BODY.md` as an example.

## Security & Configuration

Use `config.example.env` for `LIRA_*` settings. Keep credentials, local databases, backups, and binaries out of commits. Disable secure cookies only for local HTTP. Preserve per-user pocket-money ownership checks and support existing databases through additive, idempotent migrations.
