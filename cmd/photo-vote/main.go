// Command photo-vote runs the site.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sander-skjulsvik/photo-vote/internal/auth"
	"github.com/sander-skjulsvik/photo-vote/internal/config"
	"github.com/sander-skjulsvik/photo-vote/internal/store"
	"github.com/sander-skjulsvik/photo-vote/internal/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("exiting", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	st, err := store.Open(cfg.DBPath, store.Limits{
		MaxItemsPerPoll: cfg.MaxItemsPerPoll,
		MaxStorageBytes: cfg.MaxStorageBytes,
	})
	if err != nil {
		return err
	}
	defer st.Close()

	am := auth.NewManager(auth.Config{
		Issuer:       cfg.OIDCIssuer,
		ClientID:     cfg.OIDCClientID,
		ClientSecret: cfg.OIDCSecret,
		RedirectURL:  cfg.BaseURL + "/auth/callback",
		VoterGroup:   cfg.VoterGroup,
		OwnerGroup:   cfg.OwnerGroup,
		SessionTTL:   web.SessionTTL,
	})
	srv, err := web.New(web.Config{
		BaseURL:     cfg.BaseURL,
		Issuer:      cfg.OIDCIssuer,
		VoterSecret: cfg.VoterHashSecret,
		Logger:      logger,
	}, st, am)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() { errs <- httpServer.ListenAndServe() }()
	logger.Info("listening", "addr", cfg.ListenAddr)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
