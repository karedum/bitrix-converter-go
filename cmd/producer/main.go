package main

import (
	"bitrix-converter/internal/config"
	"bitrix-converter/internal/http-server/handlers/convert"
	"bitrix-converter/internal/lib/logger/sl"
	"bitrix-converter/internal/lib/rabbitmq"
	"context"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"log"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

const shutdownTimeout = 20 * time.Second

func main() {

	cfg := config.MustLoad()

	logger := sl.SetupLogger(cfg.Env)

	logger.Info("starting bitrix converter",
		slog.String("env", cfg.Env),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rabbit := rabbitmq.New(logger, cfg.Rabbit)
	if err := rabbit.Connect(); err != nil {
		log.Fatalf("failed connect to RabbitMQ with start producer [%v]", err)
	}

	go rabbit.Run(ctx)

	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.Logger)
	router.Use(middleware.Recoverer)

	router.Route("/convert", func(r chi.Router) {
		r.Post("/", convert.New(logger, rabbit))
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf("0.0.0.0:%s", cfg.APIConfig.Port),
		Handler:      router,
		ReadTimeout:  cfg.APIConfig.Timeout,
		WriteTimeout: cfg.APIConfig.Timeout,
		IdleTimeout:  cfg.APIConfig.IdleTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	logger.Info("starting producer", slog.String("addr", srv.Addr))

	select {
	case <-ctx.Done():
		logger.Info("receive a shutdown signal")
	case err := <-serverErr:
		logger.Error("failed to start producer", sl.Err(err))
	}

	stop()

	logger.Info("graceful stopping producer")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("failed to graceful stop producer", sl.Err(err))
	}

	if err := rabbit.Close(); err != nil {
		logger.Error("failed to close rabbitmq connection", sl.Err(err))
	}

	logger.Info("producer is stopped")
}
