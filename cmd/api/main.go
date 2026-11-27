package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	_ "time/tzdata" // IANA timezones even in minimal containers

	"github.com/JonathanRainee/hzzt/internal/auth"
	"github.com/JonathanRainee/hzzt/internal/categories"
	"github.com/JonathanRainee/hzzt/internal/config"
	"github.com/JonathanRainee/hzzt/internal/currency"
	"github.com/JonathanRainee/hzzt/internal/dashboard"
	"github.com/JonathanRainee/hzzt/internal/db"
	"github.com/JonathanRainee/hzzt/internal/db/sqlc"
	"github.com/JonathanRainee/hzzt/internal/goals"
	"github.com/JonathanRainee/hzzt/internal/investments"
	"github.com/JonathanRainee/hzzt/internal/manualassets"
	"github.com/JonathanRainee/hzzt/internal/marketdata"
	"github.com/JonathanRainee/hzzt/internal/marketdata/coinmarketcap"
	"github.com/JonathanRainee/hzzt/internal/marketdata/jobs"
	"github.com/JonathanRainee/hzzt/internal/marketdata/massive"
	"github.com/JonathanRainee/hzzt/internal/networth"
	"github.com/JonathanRainee/hzzt/internal/platform/email"
	"github.com/JonathanRainee/hzzt/internal/platform/events"
	"github.com/JonathanRainee/hzzt/internal/server"
	"github.com/JonathanRainee/hzzt/internal/spaces"
	"github.com/JonathanRainee/hzzt/internal/transactions"
	"github.com/JonathanRainee/hzzt/internal/transfers"
	"github.com/JonathanRainee/hzzt/internal/users"
	"github.com/JonathanRainee/hzzt/internal/wallets"
)

// @title			Hzzt API
// @version		1.0
// @description	Asset management backend API.
// @BasePath		/api/v1
// @schemes		http https
//
// @securityDefinitions.apikey	BearerAuth
// @in							header
// @name						Authorization
// @description				JWT access token: "Bearer <token>"
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if cfg.RunMigrations {
		if err := db.Migrate(ctx, pool); err != nil {
			return err
		}
	}

	log := zerolog.New(os.Stdout).With().Timestamp().Logger()

	bus := events.NewBus(log)
	var sender email.Sender = email.Console{Log: log}
	if cfg.EmailDriver == "resend" {
		sender = email.Resend{APIKey: cfg.ResendAPIKey, From: cfg.EmailFrom}
	}
	authSvc := auth.NewService(pool, auth.Options{
		AccessSecret: cfg.JWTAccessSecret,
		AccessTTL:    cfg.JWTAccessTTL,
		RefreshTTL:   time.Duration(cfg.RefreshTTLDays) * 24 * time.Hour,
		AppURL:       cfg.AppURL,
		Email:        sender,
		Events:       bus,
		Log:          log,
	})
	authHandler := auth.NewHandler(authSvc, auth.DefaultLimits(), cfg.IsProduction())

	spacesSvc := spaces.NewService(pool, spaces.Options{AppURL: cfg.AppURL, Events: bus, Log: log})
	spacesSvc.Subscribe(bus) // user.registered → default space
	spacesHandler := spaces.NewHandler(spacesSvc, authSvc)
	usersSvc := users.NewService(pool, users.Options{})
	usersSvc.Subscribe(bus) // user.registered → default preferences
	usersHandler := users.NewHandler(usersSvc, authSvc, users.DefaultLimits())

	var fxProvider currency.FxProvider = currency.NewFrankfurterProvider("")
	if cfg.FXProvider == "freecurrencyapi" {
		fxProvider = currency.NewFreeCurrencyProvider("", cfg.FreeCurrencyAPIKey)
	}
	currencySvc := currency.NewService(pool, currency.Options{Provider: fxProvider, Log: log})
	currencyHandler := currency.NewHandler(currencySvc, authSvc, spacesSvc)
	categoriesSvc := categories.NewService(pool, currencySvc)
	categoriesSvc.Subscribe(bus) // space.created → default categories
	categoriesHandler := categories.NewHandler(categoriesSvc, authSvc, spacesSvc)
	walletsSvc := wallets.NewService(pool, currencySvc, nil)
	walletsHandler := wallets.NewHandler(walletsSvc, authSvc, spacesSvc)
	transactionsSvc := transactions.NewService(pool, currencySvc, nil)
	categoriesSvc.SetUsageChecker(transactionsSvc.CategoryInUse) // locks a category's type once used
	transactionsHandler := transactions.NewHandler(transactionsSvc, authSvc, spacesSvc, walletsSvc)
	var crypto marketdata.Crypto // stays a nil interface when no key is configured
	if cfg.CMCAPIKey != "" {
		crypto = coinmarketcap.New("", cfg.CMCAPIKey)
	}
	var stocks marketdata.StockProvider // nil interfaces when no key is configured
	var strategy marketdata.PriceStrategy
	if cfg.MassiveAPIKey != "" {
		client := massive.New("", cfg.MassiveAPIKey, cfg.MassiveRatePerMinute)
		strategy, err = marketdata.NewStrategy(cfg.MassiveMode, client)
		if err != nil {
			return err
		}
		stocks = client
	}
	marketSvc := marketdata.NewService(pool, marketdata.Options{
		Crypto: crypto, Stocks: stocks, Strategy: strategy, Log: log, RefreshMinutes: cfg.CMCRefreshMinutes,
	})
	investmentsSvc := investments.NewService(pool, currencySvc, nil)
	investmentsSvc.SetMarket(marketSvc)
	goalsHandler := goals.NewHandler(goals.NewService(pool, currencySvc, nil), authSvc, spacesSvc)
	manualAssetsSvc := manualassets.NewService(pool, currencySvc, transactionsSvc, nil)
	manualAssetsHandler := manualassets.NewHandler(manualAssetsSvc, authSvc, spacesSvc)
	networthSvc := networth.NewService(pool, currencySvc, investmentsSvc, walletsSvc, manualAssetsSvc, nil)
	networthSvc.SetRunner(jobs.New(pool, log, nil))
	networthHandler := networth.NewHandler(networthSvc, authSvc, spacesSvc)
	dashboardHandler := dashboard.NewHandler(dashboard.NewService(pool, currencySvc, nil), authSvc, spacesSvc)
	stopSnapshots, err := networthSvc.StartJobs(ctx, log)
	if err != nil {
		return err
	}
	defer stopSnapshots()
	transfersHandler := transfers.NewHandler(transfers.NewService(pool, transactionsSvc, investmentsSvc, manualAssetsSvc, nil), authSvc, spacesSvc)
	investmentsHandler := investments.NewHandler(investmentsSvc, authSvc, spacesSvc, investments.DefaultLimits())
	marketHandler := marketdata.NewHandler(marketSvc, authSvc, marketdata.DefaultLimits())
	stopMarket, err := marketSvc.StartJobs(ctx, log)
	if err != nil {
		return err
	}
	defer stopMarket()
	stopJobs, err := currencySvc.StartJobs(ctx, log)
	if err != nil {
		return err
	}
	defer stopJobs()

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           server.NewRouter(cfg, log, sqlc.New(pool), authHandler.Mount, usersHandler.Mount, spacesHandler.Mount, currencyHandler.Mount, categoriesHandler.Mount, walletsHandler.Mount, transactionsHandler.Mount, transfersHandler.Mount, investmentsHandler.Mount, marketHandler.Mount, manualAssetsHandler.Mount, networthHandler.Mount, goalsHandler.Mount, dashboardHandler.Mount),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info().Str("addr", srv.Addr).Str("env", cfg.Env).Msg("server listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info().Msg("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
