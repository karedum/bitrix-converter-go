package main

import (
	"bitrix-converter/internal/config"
	"bitrix-converter/internal/lib/command"
	"bitrix-converter/internal/lib/fileuploader"
	"bitrix-converter/internal/lib/logger/sl"
	"bitrix-converter/internal/lib/rabbitmq"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	amqp "github.com/rabbitmq/amqp091-go"
	"log"
	"log/slog"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	mainPreviewQueue       = "main_preview"
	documentGeneratorQueue = "documentgenerator_create"

	consumeRetryDelay = 10 * time.Second
	// сколько ждать воркеров после принудительной отмены задач
	hardCancelTimeout = 30 * time.Second
)

var (
	queues = []string{
		mainPreviewQueue,
		documentGeneratorQueue,
	}
)

type app struct {
	log    *slog.Logger
	cfg    *config.Config
	rabbit *rabbitmq.Rabbit
	client *http.Client
	runner *command.Runner
}

func main() {

	cfg := config.MustLoad()

	logger := sl.SetupLogger(cfg.Env)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rabbit := rabbitmq.New(logger, cfg.Rabbit)

	if err := rabbit.Connect(); err != nil {
		log.Fatalf("failed connect to RabbitMQ with start %v", err)
	}

	go rabbit.Run(ctx)

	a := &app{
		log:    logger,
		cfg:    cfg,
		rabbit: rabbit,
		client: fileuploader.NewHTTPClient(cfg.Worker.HTTPTimeout),
		runner: command.NewRunner(cfg.Worker.MaxProcesses, cfg.Convert.ProcessTimeout),
	}

	workers := max(cfg.Worker.Workers, 1)

	// Задачи выполняются в контексте, который не отменяется сигналом: начатые задачи доделываются.
	// hardCancel прерывает их, если они не успели за ShutdownTimeout.
	jobCtx, hardCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer hardCancel()

	jobs := make(chan amqp.Delivery)

	// fan-in: сообщения из всех очередей сливаются в один канал jobs
	var consumersWG sync.WaitGroup
	for _, queue := range queues {
		consumersWG.Add(1)
		go func() {
			defer consumersWG.Done()
			a.consume(ctx, queue, workers, jobs)
		}()
	}

	go func() {
		consumersWG.Wait()
		close(jobs)
	}()

	// worker pool: фиксированное количество воркеров разбирает jobs
	var workersWG sync.WaitGroup
	for i := 1; i <= workers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			a.work(jobCtx, fmt.Sprintf("worker_%d", i), jobs)
		}()
	}

	logger.Info("consumer started", slog.Int("workers", workers))

	<-ctx.Done()
	stop()

	logger.Info("receive a shutdown signal, waiting for running tasks",
		slog.Duration("timeout", cfg.Worker.ShutdownTimeout))

	workersDone := make(chan struct{})
	go func() {
		workersWG.Wait()
		close(workersDone)
	}()

	select {
	case <-workersDone:
		logger.Info("graceful shutdown")
	case <-time.After(cfg.Worker.ShutdownTimeout):
		logger.Warn("shutdown timeout exceeded, cancel running tasks")
		hardCancel()
		select {
		case <-workersDone:
		case <-time.After(hardCancelTimeout):
			logger.Error("workers did not stop after cancel")
		}
	}

	if err := rabbit.Close(); err != nil {
		logger.Error("failed to close rabbitmq connection", sl.Err(err))
	}
}

// consume получает сообщения из очереди и переподключается при ошибках, пока ctx не отменён.
func (a *app) consume(ctx context.Context, queue string, prefetch int, jobs chan<- amqp.Delivery) {
	log := a.log.With(slog.String("queue", queue))

	for {
		err := a.consumeOnce(ctx, log, queue, prefetch, jobs)
		if ctx.Err() != nil {
			log.Info("consumer stopped")
			return
		}

		log.Error("consumer failed. Retry later", slog.Duration("delay", consumeRetryDelay), sl.Err(err))

		select {
		case <-ctx.Done():
			return
		case <-time.After(consumeRetryDelay):
		}
	}
}

// consumeOnce читает сообщения из одного AMQP-канала, пока канал жив или не отменён ctx.
func (a *app) consumeOnce(ctx context.Context, log *slog.Logger, queue string, prefetch int, jobs chan<- amqp.Delivery) error {
	ch, err := a.rabbit.Channel()
	if err != nil {
		return fmt.Errorf("failed to open channel: [%w]", err)
	}

	if err = a.rabbit.InitQueue(ch, queue); err != nil {
		_ = ch.Close()
		return fmt.Errorf("failed init queue: [%w]", err)
	}

	msgs, tag, err := a.rabbit.Consume(ch, queue, prefetch)
	if err != nil {
		_ = ch.Close()
		return fmt.Errorf("failed consume: [%w]", err)
	}

	log.Info("success consume. Waiting messages")

	// При остановке канал не закрываем: воркеры должны успеть подтвердить уже полученные сообщения.
	// Канал закроется вместе с соединением в конце shutdown, неподтверждённые сообщения вернутся в очередь.
	stopConsume := func() {
		if err := ch.Cancel(tag, false); err != nil {
			log.Warn("failed to cancel consumer", sl.Err(err))
		}
	}

	for {
		select {
		case <-ctx.Done():
			stopConsume()
			return nil
		case d, ok := <-msgs:
			if !ok {
				_ = ch.Close()
				return errors.New("channel closed")
			}

			select {
			case jobs <- d:
			case <-ctx.Done():
				_ = d.Nack(false, true)
				stopConsume()
				return nil
			}
		}
	}
}

func (a *app) work(ctx context.Context, workerID string, jobs <-chan amqp.Delivery) {
	for d := range jobs {
		a.handleMessage(ctx, d, workerID)
	}
}

func (a *app) handleMessage(ctx context.Context, d amqp.Delivery, workerID string) {
	const op = "consumer.handleMessage"

	log := a.log.With(
		slog.String("op", op),
		slog.String("queue", d.RoutingKey),
		slog.String("worker", workerID),
	)

	task := command.ConvertTask{}

	if err := json.Unmarshal(d.Body, &task); err != nil {
		log.Error("failed to parse body messages", sl.Err(err))
		reply(log, d.Reject(false))
		return
	}

	log = log.With(
		slog.String("request_id", task.RequestID),
		slog.String("command", task.Command),
	)

	var conv command.Converter

	switch task.Command {
	case command.DocumentCommand:
		conv = command.NewDocumentConverter(a.cfg.Convert, a.runner, workerID)
	case command.VideoCommand:
		conv = command.NewVideoConverter(a.cfg.Convert, a.runner)
	default:
		log.Error("failed to get command")
		reply(log, d.Reject(false))
		return
	}

	uploader := fileuploader.New(task.BackUrl, a.client)

	err := command.Execute(ctx, conv, task, uploader, a.cfg.Worker.UploadConcurrency)

	switch {
	case err == nil:
		reply(log, d.Ack(false))
	case ctx.Err() != nil:
		log.Warn("task interrupted by shutdown. Return message to queue", sl.Err(err))
		reply(log, d.Nack(false, true))
	default:
		log.Error("failed to exec command. Move message to dead letter queue", sl.Err(err))
		reply(log, d.Nack(false, false))
	}
}

func reply(log *slog.Logger, err error) {
	if err != nil {
		log.Error("failed to ack/nack message", sl.Err(err))
	}
}
