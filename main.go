// Command lira is a small self-hosted personal-finance app. This file is the
// composition root: it wires config -> model -> view -> controller, builds the
// router, and runs the server.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lira/internal/config"
	"lira/internal/controller"
	"lira/internal/market"
	"lira/internal/model"
	"lira/internal/view"

	_ "modernc.org/sqlite"
)

func main() {
	cfg := config.Load()

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", cfg.DBPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	// SQLite allows a single writer; serialize access to avoid SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	store := model.New(db)
	if err := store.Migrate(); err != nil {
		log.Fatalf("apply schema: %v", err)
	}
	if err := store.EnsureDefaultCategories(); err != nil {
		log.Fatalf("seed categories: %v", err)
	}
	if err := store.EnsureHousehold(); err != nil {
		log.Fatalf("ensure household: %v", err)
	}
	// Guarantee one admin (promotes the earliest user on pre-roles databases).
	if err := store.EnsureAdmin(); err != nil {
		log.Fatalf("ensure admin: %v", err)
	}
	_ = store.DeleteExpiredSessions()

	v := view.New()
	c := controller.New(store, v, cfg)

	// Market data: when a Twelve Data key is configured, build a refresh closure
	// (FX then prices) and expose it to the controller for the manual button.
	var refresh func(context.Context) error
	if cfg.MarketAPIKey != "" {
		td := market.TwelveData{APIKey: cfg.MarketAPIKey}
		refresh = func(ctx context.Context) error {
			if _, err := market.UpdateFX(ctx, td, store); err != nil {
				return err
			}
			_, err := market.UpdatePrices(ctx, td, store)
			return err
		}
		c.SetRefresh(refresh)
		log.Printf("market data: Twelve Data enabled (interval %s)", cfg.FetchInterval)
	}

	mux := http.NewServeMux()

	// Static assets
	mux.Handle("GET /static/", v.StaticHandler())

	// Public
	mux.HandleFunc("GET /login", c.Login)
	mux.HandleFunc("POST /login", c.Login)
	mux.HandleFunc("GET /register", c.Register)
	mux.HandleFunc("POST /register", c.Register)
	mux.HandleFunc("GET /invite/{token}", c.InviteAccept)
	mux.HandleFunc("POST /invite/{token}", c.InviteAccept)
	mux.HandleFunc("POST /logout", c.Logout)

	// Session required; 2FA gates handled inside
	mux.HandleFunc("GET /2fa/setup", c.SemiAuth(c.TwoFASetup))
	mux.HandleFunc("POST /2fa/setup", c.SemiAuth(c.TwoFASetup))
	mux.HandleFunc("GET /2fa/verify", c.SemiAuth(c.TwoFAVerify))
	mux.HandleFunc("POST /2fa/verify", c.SemiAuth(c.TwoFAVerify))

	// Fully authenticated app
	mux.HandleFunc("GET /{$}", c.Protect(c.Dashboard))

	mux.HandleFunc("GET /pocket", c.Protect(c.Pocket))
	mux.HandleFunc("POST /pocket", c.Protect(c.PocketAdd))
	mux.HandleFunc("POST /pocket/{id}/delete", c.Protect(c.PocketDelete))

	mux.HandleFunc("POST /tokens", c.Protect(c.TokenCreate))
	mux.HandleFunc("POST /tokens/{id}/delete", c.Protect(c.TokenRevoke))

	// JSON API (v1): bearer-token auth, separate from the cookie/CSRF web app.
	mux.HandleFunc("GET /api/v1/me", c.APIAuth(c.APIMe))
	mux.HandleFunc("GET /api/v1/summary", c.APIAuth(c.APISummary))
	mux.HandleFunc("GET /api/v1/accounts", c.APIAuth(c.APIAccounts))
	mux.HandleFunc("POST /api/v1/accounts", c.APIAuth(c.APICreateAccount))
	mux.HandleFunc("GET /api/v1/transactions", c.APIAuth(c.APITransactions))
	mux.HandleFunc("POST /api/v1/transactions", c.APIAuth(c.APICreateTransaction))
	mux.HandleFunc("DELETE /api/v1/transactions/{id}", c.APIAuth(c.APIDeleteTransaction))
	mux.HandleFunc("GET /api/v1/holdings", c.APIAuth(c.APIHoldings))
	mux.HandleFunc("POST /api/v1/holdings", c.APIAuth(c.APICreateHolding))
	mux.HandleFunc("GET /api/v1/projects", c.APIAuth(c.APIProjects))
	mux.HandleFunc("GET /api/v1/costs", c.APIAuth(c.APICosts))
	mux.HandleFunc("GET /api/v1/pocket", c.APIAuth(c.APIPocket))
	mux.HandleFunc("POST /api/v1/pocket", c.APIAuth(c.APICreatePocket))
	mux.HandleFunc("DELETE /api/v1/pocket/{id}", c.APIAuth(c.APIDeletePocket))

	mux.HandleFunc("GET /reports", c.Protect(c.Reports))
	mux.HandleFunc("GET /history", c.Protect(c.NavHistory))
	mux.HandleFunc("GET /forecast", c.Protect(c.NavForecast))
	mux.HandleFunc("GET /burndown", c.Protect(c.BurnDown))
	mux.HandleFunc("POST /snapshot", c.Protect(c.SnapshotNow))
	mux.HandleFunc("POST /refresh", c.Protect(c.RefreshNow))

	mux.HandleFunc("GET /insights", c.Protect(c.Insights))

	mux.HandleFunc("GET /costs", c.Protect(c.Costs))
	mux.HandleFunc("GET /costs/new", c.Protect(c.CostNew))
	mux.HandleFunc("POST /costs", c.Protect(c.CostCreate))
	mux.HandleFunc("POST /costs/income", c.Protect(c.IncomeUpdate))
	mux.HandleFunc("GET /costs/{id}/edit", c.Protect(c.CostEdit))
	mux.HandleFunc("POST /costs/{id}", c.Protect(c.CostUpdate))
	mux.HandleFunc("POST /costs/{id}/toggle", c.Protect(c.CostToggle))
	mux.HandleFunc("POST /costs/{id}/delete", c.Protect(c.CostDelete))

	mux.HandleFunc("GET /accounts", c.Protect(c.Accounts))
	mux.HandleFunc("GET /accounts/new", c.Protect(c.AccountNew))
	mux.HandleFunc("POST /accounts", c.Protect(c.AccountCreate))
	mux.HandleFunc("GET /accounts/{id}/edit", c.Protect(c.AccountEdit))
	mux.HandleFunc("POST /accounts/{id}", c.Protect(c.AccountUpdate))

	mux.HandleFunc("GET /transactions", c.Protect(c.Transactions))
	mux.HandleFunc("GET /transactions/export", c.Protect(c.ExportTransactions))
	mux.HandleFunc("GET /holdings/export", c.Protect(c.ExportHoldings))
	mux.HandleFunc("GET /projection", c.Protect(c.Projection))
	mux.HandleFunc("GET /costs/export", c.Protect(c.ExportCosts))
	mux.HandleFunc("GET /transactions/import", c.Protect(c.ImportUpload))
	mux.HandleFunc("POST /transactions/import/preview", c.Protect(c.ImportPreview))
	mux.HandleFunc("POST /transactions/import/commit", c.Protect(c.ImportCommit))
	mux.HandleFunc("GET /transactions/new", c.Protect(c.TransactionNew))
	mux.HandleFunc("POST /transactions", c.Protect(c.TransactionCreate))
	mux.HandleFunc("GET /transactions/{id}/edit", c.Protect(c.TransactionEdit))
	mux.HandleFunc("POST /transactions/{id}", c.Protect(c.TransactionUpdate))
	mux.HandleFunc("POST /transactions/{id}/delete", c.Protect(c.TransactionDelete))

	mux.HandleFunc("GET /categories", c.Protect(c.Categories))
	mux.HandleFunc("POST /categories", c.Protect(c.CategoryCreate))
	mux.HandleFunc("GET /categories/{id}/edit", c.Protect(c.CategoryEdit))
	mux.HandleFunc("POST /categories/{id}", c.Protect(c.CategoryUpdate))
	mux.HandleFunc("POST /categories/{id}/delete", c.Protect(c.CategoryDelete))
	mux.HandleFunc("GET /transfer", c.Protect(c.TransferNew))
	mux.HandleFunc("POST /transfer", c.Protect(c.TransferCreate))

	mux.HandleFunc("GET /holdings", c.Protect(c.Holdings))
	mux.HandleFunc("GET /holdings/new", c.Protect(c.HoldingNew))
	mux.HandleFunc("POST /holdings", c.Protect(c.HoldingCreate))
	mux.HandleFunc("GET /holdings/{id}/edit", c.Protect(c.HoldingEdit))
	mux.HandleFunc("POST /holdings/{id}", c.Protect(c.HoldingUpdate))

	mux.HandleFunc("GET /projects", c.Protect(c.Projects))
	mux.HandleFunc("GET /projects/new", c.Protect(c.ProjectNew))
	mux.HandleFunc("POST /projects", c.Protect(c.ProjectCreate))
	mux.HandleFunc("GET /projects/{id}", c.Protect(c.ProjectView))
	mux.HandleFunc("GET /projects/{id}/plan", c.Protect(c.ProjectPlan))
	mux.HandleFunc("GET /projects/{id}/edit", c.Protect(c.ProjectEdit))
	mux.HandleFunc("POST /projects/{id}", c.Protect(c.ProjectUpdate))
	mux.HandleFunc("POST /projects/{id}/status", c.Protect(c.ProjectStatus))
	mux.HandleFunc("POST /projects/{id}/loan", c.Protect(c.LoanUpdate))
	mux.HandleFunc("POST /projects/{id}/deposit", c.Protect(c.DepositPay))
	mux.HandleFunc("POST /projects/{id}/delete", c.Protect(c.ProjectDelete))
	mux.HandleFunc("POST /projects/{id}/phases", c.Protect(c.PhaseCreate))
	mux.HandleFunc("GET /projects/{id}/phases/{phaseID}/edit", c.Protect(c.PhaseEdit))
	mux.HandleFunc("POST /projects/{id}/phases/{phaseID}", c.Protect(c.PhaseUpdate))
	mux.HandleFunc("POST /projects/{id}/phases/{phaseID}/delete", c.Protect(c.PhaseDelete))
	mux.HandleFunc("POST /projects/{id}/items", c.Protect(c.ItemCreate))
	mux.HandleFunc("GET /projects/{id}/items/{itemID}/edit", c.Protect(c.ItemEdit))
	mux.HandleFunc("POST /projects/{id}/items/{itemID}", c.Protect(c.ItemUpdate))
	mux.HandleFunc("POST /projects/{id}/items/{itemID}/delete", c.Protect(c.ItemDelete))
	mux.HandleFunc("POST /projects/{id}/items/{itemID}/move", c.Protect(c.ItemMove))
	mux.HandleFunc("GET /projects/{id}/items/{itemID}/purchase", c.Protect(c.ItemPurchaseForm))
	mux.HandleFunc("POST /projects/{id}/items/{itemID}/purchase", c.Protect(c.ItemPurchase))

	// Settings (admin only): household name + invites
	mux.HandleFunc("GET /settings", c.Protect(c.AdminOnly(c.Settings)))
	mux.HandleFunc("POST /settings/household", c.Protect(c.AdminOnly(c.HouseholdRename)))
	mux.HandleFunc("POST /settings/savings", c.Protect(c.AdminOnly(c.SavingsUpdate)))
	mux.HandleFunc("POST /settings/invite", c.Protect(c.AdminOnly(c.InviteCreate)))
	mux.HandleFunc("POST /settings/invite/{id}/delete", c.Protect(c.AdminOnly(c.InviteRevoke)))
	mux.HandleFunc("GET /backup", c.Protect(c.AdminOnly(c.BackupDownload)))

	handler := securityHeaders(recoverMW(logMW(mux)))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Background maintenance: refresh market data (if enabled), then record one
	// NAV snapshot per day (deduped) — on boot and every fetch interval.
	go func() {
		interval := cfg.FetchInterval
		if interval <= 0 {
			interval = 24 * time.Hour
		}
		tick := func() {
			// A panic here would otherwise kill the whole process (this runs
			// outside the HTTP recover middleware); contain it to one cycle.
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("maintenance panic: %v", rec)
				}
			}()
			// Cap automatic fetches at once per UTC day so restarts/redeploys
			// don't burn API credits. The manual /refresh button is the override.
			if refresh != nil && !store.AutoFetchedToday() {
				rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if err := refresh(rctx); err != nil {
					log.Printf("market refresh: %v", err)
				} else {
					_ = store.MarkAutoFetched()
				}
				cancel()
			}
			if err := store.EnsureDailySnapshot(); err != nil {
				log.Printf("nav snapshot: %v", err)
			}
		}
		tick()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tick()
			}
		}
	}()

	go func() {
		log.Printf("Lira listening on %s (secure_cookies=%v, db=%s)", cfg.Addr, cfg.SecureCookies, cfg.DBPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// ---------- generic middleware ----------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'none'; "+
				"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; "+
				"font-src https://fonts.gstatic.com; img-src 'self' data:; "+
				"object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
