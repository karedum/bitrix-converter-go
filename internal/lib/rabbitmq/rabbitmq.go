package rabbitmq

import (
	"bitrix-converter/internal/config"
	"bitrix-converter/internal/lib/logger/sl"
	"context"
	"fmt"
	amqp "github.com/rabbitmq/amqp091-go"
	"log/slog"
	"time"
)

type Rabbit struct {
	conn *amqp.Connection
	log  *slog.Logger
	cfg  config.RabbitConfig
}

func New(log *slog.Logger, cfg config.RabbitConfig) *Rabbit {
	return &Rabbit{
		log: log,
		cfg: cfg,
	}
}

func (r *Rabbit) DefaultQueue() string {
	return r.cfg.DefaultQueue
}

func (r *Rabbit) Connect() error {
	url := fmt.Sprintf("amqp://%s:%s@%s:%s", r.cfg.User, r.cfg.Password, r.cfg.Host, r.cfg.Port)

	conn, err := amqp.Dial(url)
	if err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: [%w]", err)
	}

	r.conn = conn
	return nil
}

func (r *Rabbit) Channel() (*amqp.Channel, error) {
	return r.conn.Channel()
}

func (r *Rabbit) Reconnect() {
	for {
		_, ok := <-r.conn.NotifyClose(make(chan *amqp.Error))
		if !ok {
			r.log.Error("failed notifying rabbitMQ channel. Reconnecting...")
		}
		r.log.Error("rabbitmq connection closed unexpectedly. Reconnecting...")

		for {

			err := r.Connect()

			if err == nil {
				r.log.Info("rabbitMQ reconnect success")
				break
			}

			r.log.Error("rabbitmq reconnect failed. Retry after 10 seconds", sl.Err(err))
			time.Sleep(10 * time.Second)
		}

	}
}

func (r *Rabbit) InitQueue(ch *amqp.Channel, queue string) error {

	dlQueue := queue + "_dead"

	_, err := ch.QueueDeclare(
		dlQueue,
		true,
		false,
		false,
		false,
		amqp.Table{},
	)

	if err != nil {
		return fmt.Errorf("failed to declare dead letter queue: [%w]", err)
	}

	_, err = ch.QueueDeclare(
		queue,
		true,
		false,
		false,
		false,
		amqp.Table{
			"x-dead-letter-exchange":    "",
			"x-dead-letter-routing-key": dlQueue,
			"x-message-ttl":             60480000,
		},
	)

	if err != nil {
		return fmt.Errorf("failed to declare queue: [%w]", err)
	}
	return nil
}

func (r *Rabbit) Consume(ch *amqp.Channel, queue string) (msgs <-chan amqp.Delivery, err error) {
	msgs, err = ch.Consume(
		queue,
		"",
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to consume queue: [%w]", err)
	}
	return msgs, nil
}

func (r *Rabbit) Publish(queue string, message []byte) error {
	ch, err := r.Channel()
	if err != nil {
		return fmt.Errorf("failed to open channel: [%w]", err)
	}

	defer ch.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = ch.PublishWithContext(
		ctx,
		"",
		queue,
		false,
		false,
		amqp.Publishing{
			ContentType: "text/plain",
			Body:        message,
		},
	)

	if err != nil {
		return fmt.Errorf("failed to publish message: [%w]", err)
	}
	return nil
}

func (r *Rabbit) Connection() *amqp.Connection {
	return r.conn
}
