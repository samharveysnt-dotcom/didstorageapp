// Command didapi is the single Go binary that runs everything except the
// scheduled billing job: admin GUI, reseller API, and the Kamailio control
// plane (/sipctl/authorize and /sipctl/cdr).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alexedwards/scs/postgresstore"
	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"didstorage/internal/auth"
	"didstorage/internal/causes"
	"didstorage/internal/config"
	"didstorage/internal/db"
	"didstorage/internal/livecalls"
	"didstorage/internal/resellerapi"
	"didstorage/internal/settings"
	"didstorage/internal/sipctl"
	"didstorage/internal/sslmgr"
	"didstorage/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pg, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db: %w", err)
	}
	defer pg.Close()
	logger.Info("postgres connected")

	if adminPW := os.Getenv("ADMIN_PASSWORD"); adminPW != "" {
		if err := auth.EnsureAdmin(ctx, pg.Pool, adminPW); err != nil {
			return fmt.Errorf("ensure admin: %w", err)
		}
		logger.Info("admin password applied from env")
	}

	rOpt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("redis url: %w", err)
	}
	rdb := redis.NewClient(rOpt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	defer rdb.Close()
	logger.Info("redis connected")

	// Hangup-cause table → in-memory map, hot on every CDR row render.
	// Failure here is non-fatal: the lookup just returns "code, ''" so the
	// CDR list still works (label = code, no tooltip detail).
	if err := causes.Reload(ctx, pg.Pool); err != nil {
		logger.Warn("hangup_causes load failed (lookups will fall back to code-as-label)", "err", err)
	}
	// Site / company settings.
	if err := settings.Reload(ctx, pg.Pool); err != nil {
		logger.Warn("settings load failed", "err", err)
	}
	// TLS cert manager — populates from site_domains. IsConfigured() drives
	// whether we start the HTTPS listener below.
	ssl := sslmgr.New(pg.Pool, logger.With("component", "sslmgr"))
	if err := ssl.Reload(ctx); err != nil {
		logger.Warn("sslmgr load failed (HTTPS listener disabled until configured)", "err", err)
	}

	// Sessions: scs with Postgres backing store, sharing the pgx pool via
	// stdlib *sql.DB adapter (postgresstore wants database/sql).
	sqlDB := stdlib.OpenDBFromPool(pg.Pool)
	defer sqlDB.Close()

	sm := scs.New()
	sm.Store = postgresstore.New(sqlDB)
	sm.Lifetime = 12 * time.Hour
	sm.IdleTimeout = 2 * time.Hour
	sm.Cookie.Name = "didstorage_session"
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Persist = true

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	// 90s cap. Most requests return in <100ms; the long tail is SIP-trace
	// lookups, which fully parse multi-GB rolling pcaps with tshark. We parallelize
	// inside siptrace, but the worst case still lands around 30s when several
	// big pcaps match the search prefix.
	r.Use(middleware.Timeout(90 * time.Second))

	// Healthcheck — outside session middleware so probes don't get cookies.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	// Kamailio control plane — outside session middleware (token-auth instead).
	sipctl.SetPublicIP(cfg.PublicIP)
	sipctlH := &sipctl.Handler{
		DB:                    pg,
		Redis:                 rdb,
		AuthToken:             cfg.KamailioAuthToken,
		Log:                   logger.With("component", "sipctl"),
		MinSecondsToAuthorize: cfg.MinAuthorizeSeconds,
	}
	mux := http.NewServeMux()
	sipctlH.Routes(mux)
	r.Mount("/sipctl", http.StripPrefix("/sipctl", mux))

	// Reseller REST API (bearer-token authenticated). Redis powers /api/v1/live.
	// Resellers are frontend-only distributors with no server-side
	// infrastructure (see PRODUCT.md "Resellers and the API model"): no
	// webhooks, no callbacks, no push channel. End users poll our API via
	// the reseller's branded UI.
	rapi := resellerapi.New(pg, rdb, logger.With("component", "resellerapi"), cfg.PublicIP)
	rapi.Mount(r)

	// Admin GUI — wrapped in scs LoadAndSave middleware.
	webH, err := web.New(pg, rdb, sm, logger.With("component", "web"), cfg.PublicIP)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}
	webH.SSL = ssl
	// Ensure /etc/asterisk/pjsip_suppliers.conf reflects DB state on every
	// boot. Async so a slow Asterisk reload doesn't block the listener
	// coming up. Mutations through /suppliers/* handlers also re-trigger
	// this on every change.
	go webH.RegenSupplierIdentifiesStartup()

	// Live-calls reconciler: every 2s, ask Asterisk which channels are
	// still alive and sweep any live-calls index entry whose channel
	// isn't. Backstop for the primary path (dialplan hangup handler →
	// dids-cdr.py → /sipctl/cdr → Deregister) when that path fails to
	// fire — a timed-out AGI POST, an abrupt channel destroy, a race
	// with a didapi restart. Also releases the corresponding
	// channel-reservation set memberships so the concurrency-cap check
	// in /sipctl/authorize doesn't over-count against ghost calls.
	//
	// 2s tick keeps ghost visibility ≤2-3s end-to-end. Settle window is
	// 5s (>2× tick) so we never sweep a call during its own admission
	// race between /sipctl/authorize and the first `core show channels`
	// snapshot that includes its channel — in practice the channel is
	// always visible immediately (the AGI runs on it) but the buffer
	// costs nothing.
	//
	// `asterisk -rx "core show channels concise"` is a ~5ms local
	// subprocess; 30 calls/min is negligible CPU on this box.
	livecalls.StartReconciler(ctx, rdb, livecalls.ReconcilerOptions{
		TickInterval: 1 * time.Second,
		SettleWindow: 3 * time.Second,
		ReleaseReservations: func(ctx context.Context, callIDs []string) error {
			return livecalls.ReleaseChannelReservations(ctx, rdb, callIDs)
		},
		Log: logger.With("component", "livecalls-reconciler"),
	})
	r.Group(func(r chi.Router) {
		r.Use(sm.LoadAndSave)
		webH.Mount(r)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("didapi listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen failed", "err", err)
			cancel()
		}
	}()

	// Optional HTTPS listener. We bring it up if (a) the admin set
	// site.https_listen_addr to a non-empty value AND (b) at least one
	// domain row has a usable cert. Cert lookup is SNI-driven via the
	// sslmgr GetCertificate callback.
	httpsAddr := settings.GetWithDefault("site.https_listen_addr", "")
	var tlsSrv *http.Server
	if httpsAddr != "" && ssl.IsConfigured() {
		tlsSrv = &http.Server{
			Addr:              httpsAddr,
			Handler:           r,
			ReadHeaderTimeout: 5 * time.Second,
			TLSConfig: &tls.Config{
				MinVersion:     tls.VersionTLS12,
				GetCertificate: ssl.GetCertificate,
			},
		}
		go func() {
			logger.Info("didapi https listening", "addr", httpsAddr)
			if err := tlsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// Non-fatal — log and let HTTP keep serving.
				logger.Error("https listen failed (HTTP still serving)", "err", err)
			}
		}()
	} else if httpsAddr != "" {
		logger.Info("https listener skipped (no cert configured)", "addr", httpsAddr)
	}

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if tlsSrv != nil {
		_ = tlsSrv.Shutdown(shutdownCtx)
	}
	return srv.Shutdown(shutdownCtx)
}
