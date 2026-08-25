// Command server runs the configurator and the CI worker in one process.
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

	"github.com/openpreflight/openpreflight/internal/api"
	"github.com/openpreflight/openpreflight/internal/config"
	"github.com/openpreflight/openpreflight/internal/queue"
	"github.com/openpreflight/openpreflight/internal/secret"
	"github.com/openpreflight/openpreflight/internal/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		if errors.Is(err, config.ErrNoSecretKey) {
			// Refusing to start is the point: booting without the key would
			// leave every stored PEM and token unreadable, silently.
			return fmt.Errorf("%w\n\nGenerate one with:  openssl rand -base64 48", err)
		}
		return err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}

	box, err := secret.New(cfg.SecretKey)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath(), box)
	if err != nil {
		return err
	}
	defer st.Close()

	if cfg.OldSecretKey != "" {
		old, err := secret.New(cfg.OldSecretKey)
		if err != nil {
			return fmt.Errorf("CI_SECRET_KEY_OLD: %w", err)
		}
		n, err := st.RotateSecrets(old)
		if err != nil {
			return fmt.Errorf("rotate secrets: %w", err)
		}
		log.Info("re-sealed secret columns under CI_SECRET_KEY", "count", n)
		log.Warn("unset CI_SECRET_KEY_OLD and restart so the previous key is not left in the environment")
	}

	if err := bootstrapAdmin(st, cfg, log); err != nil {
		return err
	}
	if err := seedPublicURL(st, cfg, log); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runner := queue.New(st, cfg, log)
	go runner.Start(ctx)

	server, err := api.New(st, cfg, runner, log)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: server.Handler(),
		// A build log page can be large and a webhook body arrives fast; these
		// are generous enough for both without letting a socket idle forever.
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "data_dir", cfg.DataDir,
			"workspace_dir", cfg.WorkspaceDir())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Stop accepting requests first, then let in-flight jobs notice the
	// cancelled context and mark themselves cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "error", err)
	}
	return nil
}

// bootstrapAdmin seeds the admin from the environment on first boot so a
// headless deployment can be driven by API without the browser wizard.
func bootstrapAdmin(st *store.Store, cfg config.Config, log *slog.Logger) error {
	if cfg.BootstrapAdminPassword == "" {
		return nil
	}
	hasUsers, err := st.HasUsers()
	if err != nil {
		return err
	}
	if hasUsers {
		log.Info("CI_BOOTSTRAP_ADMIN_PASSWORD ignored: an admin already exists")
		return nil
	}
	if _, err := st.CreateUser("admin", cfg.BootstrapAdminPassword); err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	log.Info("created admin user from CI_BOOTSTRAP_ADMIN_PASSWORD", "username", "admin")
	return nil
}

// seedPublicURL fills the settings row from the environment on first boot only.
// After that the UI owns the value.
func seedPublicURL(st *store.Store, cfg config.Config, log *slog.Logger) error {
	if cfg.PublicBaseURL == "" {
		return nil
	}
	settings, err := st.Settings()
	if err != nil {
		return err
	}
	if settings.PublicBaseURL != "" {
		return nil
	}
	settings.PublicBaseURL = cfg.PublicBaseURL
	if err := st.SaveSettings(settings); err != nil {
		return err
	}
	log.Info("seeded public base URL from CI_PUBLIC_BASE_URL", "url", cfg.PublicBaseURL)
	return nil
}
