package main

import (
	"bitrix-converter/internal/config"
	"bitrix-converter/internal/lib/command"
	"bitrix-converter/internal/lib/fileuploader"
	"bitrix-converter/internal/lib/logger/sl"
	"bitrix-converter/internal/lib/rabbitmq"
	"context"
	"encoding/json"
	"fmt"
	amqp "github.com/rabbitmq/amqp091-go"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	mainPreviewQueue       = "main_preview"
	documentGeneratorQueue = "documentgenerator_create"
)

var (
	queues = []string{
		mainPreviewQueue,
		documentGeneratorQueue,
	}
)

func main() {

	cfg := config.MustLoad()

	logger := sl.SetupLogger(cfg.Env)

	rabbit := rabbitmq.New(logger, cfg.Rabbit)

	conErr := rabbit.Connect()
	if conErr != nil {
		log.Fatalf("failed connect to RabbitMQ with start %v", conErr)
	}

	go rabbit.Reconnect()

	cancelCtx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	for i := 1; i <= 3; i++ {
		for _, queue := range queues {

			go func() {
				uniqId := fmt.Sprintf("%s_%d", queue, i)
				logger.Info("start consumer", slog.String("queue", queue))
				wg.Add(1)
			done:
				for {
					time.Sleep(10 * time.Second)
					ch, err := rabbit.Channel()
					if err != nil {
						logger.Error("failed to open channel. Retry", slog.String("queue", queue), sl.Err(err))
						continue
					}
					defer ch.Close()

					err = rabbit.InitQueue(ch, queue)

					if err != nil {
						logger.Error("failed init queue. Retry", slog.String("queue", queue), sl.Err(err))
						continue
					}

					logger.Info("success init queue", slog.String("queue", queue))
					msgs, err := rabbit.Consume(ch, queue)

					if err != nil {
						logger.Error("failed consume. Retry", slog.String("queue", queue), sl.Err(err))
						continue
					}

					logger.Info("success consume. Waiting messages", slog.String("queue", queue))
				closed:
					for {
						select {
						case <-cancelCtx.Done():
							break done
						default:
						}
						select {
						case d, ok := <-msgs:
							if !ok {
								logger.Info("channel closed", slog.String("queue", queue))
								break closed
							}
							handleMessage(d, logger, cfg, uniqId)
						default:
						}
					}
				}
				wg.Done()
			}()

		}
	}
	waitCh := make(chan struct{})
	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGINT, syscall.SIGKILL)

	<-ch
	logger.Info("receive a shutdown signal")
	go func() {
		logger.Info("cancel, wait consumer")
		cancel()
		wg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		logger.Info("graceful shutdown")
	case <-time.After(5 * time.Minute):
		logger.Info("shutdown before 5 minutes timeout")
	}

}

func handleMessage(d amqp.Delivery, log *slog.Logger, cfg *config.Config, uniqId string) {
	const op = "consumer.handleMessage"

	task := command.ConvertTask{}
	queue := d.RoutingKey

	err := json.Unmarshal(d.Body, &task)
	if err != nil {
		log.Error("failed to parse body messages", slog.String("queue", queue), sl.Err(err))
		_ = d.Reject(false)
		return
	}

	log = log.With(
		slog.String("op", op),
		slog.String("request_id", task.RequestID),
	)

	uploader := fileuploader.New(task.BackUrl)
	var cmd command.Command

	switch task.Command {
	case "Bitrix\\TransformerController\\Document":
		cmd = command.NewDocumentCommand(task, log, *uploader, cfg.Convert, uniqId)
	case "Bitrix\\TransformerController\\Video":
		cmd = command.NewVideoCommand(task, log, *uploader, cfg.Convert)
	default:
		log.Error("failed to get command",
			slog.String("queue", queue),
			slog.String("command", task.Command),
			sl.Err(err))
		_ = d.Reject(false)
		return
	}

	err = cmd.Execute()
	if err != nil {
		log.Error("failed to exec command",
			slog.String("queue", queue),
			slog.String("command", task.Command),
			sl.Err(err))
	}
	_ = d.Ack(false)
}
