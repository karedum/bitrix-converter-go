package main

import (
	"bitrix-converter/internal/config"
	"bitrix-converter/internal/lib/logger/sl"
	"bitrix-converter/internal/lib/rabbitmq"
	"context"
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"log"

	"bitrix-converter/internal/http-server/handlers/convert"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {

	cfg := config.MustLoad()

	logger := sl.SetupLogger(cfg.Env)

	logger.Info("starting bitrix converter",
		slog.String("env", cfg.Env),
	)

	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.Logger)

	ctx := context.WithoutCancel(context.Background())

	rabbit := rabbitmq.New(logger, cfg.Rabbit)
	err := rabbit.Connect()
	if err != nil {
		log.Fatalf("failed connect to RabbitMQ with start producer %v", err)
		return
	}

	go rabbit.Reconnect()

	router.Route("/convert", func(r chi.Router) {
		r.Post("/", convert.New(ctx, logger, rabbit))
	})

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	srv := &http.Server{
		Addr:         fmt.Sprintf("0.0.0.0:%s", cfg.APIConfig.Port),
		Handler:      router,
		ReadTimeout:  cfg.APIConfig.Timeout,
		WriteTimeout: cfg.APIConfig.Timeout,
		IdleTimeout:  cfg.APIConfig.IdleTimeout,
	}

	go func() {
		if err = srv.ListenAndServe(); err != nil {
			logger.Error("failed to start producer", sl.Err(err))
		}
	}()

	logger.Info("starting producer")

	<-done

	logger.Info("graceful stopping producer")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err = srv.Shutdown(ctx); err != nil {
		logger.Error("failed to graceful stop producer", sl.Err(err))
		return
	}

	logger.Info("producer is stopped")

}
