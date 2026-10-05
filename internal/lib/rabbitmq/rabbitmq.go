package rabbitmq

import (
	"bitrix-converter/internal/config"
	"bitrix-converter/internal/lib/logger/sl"
	"context"
	"errors"
	"fmt"
	amqp "github.com/rabbitmq/amqp091-go"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	reconnectDelay = 10 * time.Second
	publishTimeout = 5 * time.Second
)

var ErrNotConnected = errors.New("rabbitmq is not connected")

type Rabbit struct {
	log *slog.Logger
	cfg config.RabbitConfig

	mu   sync.RWMutex
	conn *amqp.Connection

	// pubMu защищает pubCh: amqp.Channel нельзя использовать для публикации из нескольких горутин одновременно
	pubMu sync.Mutex
	pubCh *amqp.Channel

	closed      atomic.Bool
	consumerSeq atomic.Uint64
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

	r.mu.Lock()
	r.conn = conn
	r.mu.Unlock()
	return nil
}

func (r *Rabbit) connection() (*amqp.Connection, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.conn == nil || r.conn.IsClosed() {
		return nil, ErrNotConnected
	}
	return r.conn, nil
}

func (r *Rabbit) Channel() (*amqp.Channel, error) {
	conn, err := r.connection()
	if err != nil {
		return nil, err
	}
	return conn.Channel()
}

// Run следит за соединением и переподключается при его разрыве. Завершается при отмене ctx.
func (r *Rabbit) Run(ctx context.Context) {
	for {
		if conn, err := r.connection(); err == nil {
			closeCh := conn.NotifyClose(make(chan *amqp.Error, 1))
			select {
			case <-ctx.Done():
				return
			case amqpErr := <-closeCh:
				if r.closed.Load() {
					return
				}
				r.log.Error("rabbitmq connection closed unexpectedly. Reconnecting...", slog.Any("reason", amqpErr))
			}
		}

		for {
			err := r.Connect()
			if err == nil {
				r.log.Info("rabbitMQ reconnect success")
				break
			}

			r.log.Error("rabbitmq reconnect failed. Retry later", slog.Duration("delay", reconnectDelay), sl.Err(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectDelay):
			}
		}
	}
}

// Close закрывает канал публикации и соединение. После Close переподключение не выполняется.
func (r *Rabbit) Close() error {
	r.closed.Store(true)

	r.pubMu.Lock()
	if r.pubCh != nil {
		_ = r.pubCh.Close()
		r.pubCh = nil
	}
	r.pubMu.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil || r.conn.IsClosed() {
		return nil
	}
	return r.conn.Close()
}

func (r *Rabbit) InitQueue(ch *amqp.Channel, queue string) error {

	dlQueue := queue + "_dead"

	_, err := ch.QueueDeclare(
		dlQueue,
		true,
		false,
		false,
		false,
		amqp.Table{
			"x-message-ttl": 60480000,
		},
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
		},
	)

	if err != nil {
		return fmt.Errorf("failed to declare queue: [%w]", err)
	}
	return nil
}

// Consume начинает получение сообщений из очереди. prefetch ограничивает количество неподтверждённых
// сообщений, которые брокер отдаёт каналу. Возвращает consumer tag для остановки через ch.Cancel.
func (r *Rabbit) Consume(ch *amqp.Channel, queue string, prefetch int) (<-chan amqp.Delivery, string, error) {
	if err := ch.Qos(prefetch, 0, false); err != nil {
		return nil, "", fmt.Errorf("failed to set qos: [%w]", err)
	}

	tag := fmt.Sprintf("%s-%d-%d", queue, os.Getpid(), r.consumerSeq.Add(1))

	msgs, err := ch.Consume(
		queue,
		tag,
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return nil, "", fmt.Errorf("failed to consume queue: [%w]", err)
	}
	return msgs, tag, nil
}

// Publish публикует persistent-сообщение и ждёт подтверждения от брокера.
func (r *Rabbit) Publish(ctx context.Context, queue string, message []byte) error {
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	confirm, err := r.publish(ctx, queue, message)
	if err != nil {
		return err
	}

	acked, err := confirm.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to wait publish confirmation: [%w]", err)
	}
	if !acked {
		return errors.New("message was not acknowledged by broker")
	}
	return nil
}

func (r *Rabbit) publish(ctx context.Context, queue string, message []byte) (*amqp.DeferredConfirmation, error) {
	r.pubMu.Lock()
	defer r.pubMu.Unlock()

	if r.pubCh == nil || r.pubCh.IsClosed() {
		ch, err := r.Channel()
		if err != nil {
			return nil, fmt.Errorf("failed to open channel: [%w]", err)
		}
		if err = ch.Confirm(false); err != nil {
			_ = ch.Close()
			return nil, fmt.Errorf("failed to enable publisher confirms: [%w]", err)
		}
		r.pubCh = ch
	}

	confirm, err := r.pubCh.PublishWithDeferredConfirmWithContext(
		ctx,
		"",
		queue,
		false,
		false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         message,
		},
	)

	if err != nil {
		_ = r.pubCh.Close()
		r.pubCh = nil
		return nil, fmt.Errorf("failed to publish message: [%w]", err)
	}
	return confirm, nil
}
