package config

import (
	"github.com/ilyakaznacheev/cleanenv"
	"log"
	"time"
)

type Config struct {
	Env       string `env:"CONVERT_ENV"`
	APIConfig APIConfig
	Rabbit    RabbitConfig
	Convert   ConvertConfig
	Worker    WorkerConfig
}

type ConvertConfig struct {
	SuccessDir      string        `env:"CONVERT_SUCCESS_DIRECTORY"`
	DownloadDir     string        `env:"CONVERT_DOWNLOAD_DIRECTORY"`
	MaxVideoSize    int64         `env:"CONVERT_MAX_VIDEO_SIZE"`
	MaxDocumentSize int64         `env:"CONVERT_MAX_DOCUMENT_SIZE"`
	ProcessTimeout  time.Duration `env:"CONVERT_PROCESS_TIMEOUT" env-default:"10m"`
}

type WorkerConfig struct {
	// Workers - количество воркеров, одновременно обрабатывающих задачи из всех очередей
	Workers int `env:"CONVERT_WORKERS" env-default:"6"`
	// MaxProcesses - максимум одновременно запущенных libreoffice/ffmpeg/convert. 0 - по числу CPU
	MaxProcesses int `env:"CONVERT_MAX_PROCESSES" env-default:"0"`
	// UploadConcurrency - сколько сконвертированных файлов одной задачи загружаются параллельно.
	// 1 - по одному: при параллельной загрузке в одну задачу Битрикс иногда отвечает пустым телом.
	UploadConcurrency int `env:"CONVERT_UPLOAD_CONCURRENCY" env-default:"1"`
	// ShutdownTimeout - сколько ждать завершения текущих задач при остановке
	ShutdownTimeout time.Duration `env:"CONVERT_SHUTDOWN_TIMEOUT" env-default:"4m30s"`
	// HTTPTimeout - таймаут одного HTTP-запроса к Битрикс
	HTTPTimeout time.Duration `env:"CONVERT_HTTP_TIMEOUT" env-default:"5m"`
}

type RabbitConfig struct {
	User         string `env:"RABBITMQ_USER" env-required:"true"`
	Password     string `env:"RABBITMQ_PASSWORD"`
	Host         string `env:"RABBITMQ_HOST" env-required:"true"`
	Port         string `env:"RABBITMQ_PORT" env-required:"true"`
	DefaultQueue string `env:"RABBITMQ_DEFAULT_QUEUE" env-required:"true"`
}

type APIConfig struct {
	Port        string        `env:"CONVERT_API_PORT" env-default:"8100"`
	Timeout     time.Duration `env:"CONVERT_API_TIMEOUT" env-default:"30s"`
	IdleTimeout time.Duration `env:"CONVERT_API_IDLE_TIMEOUT" env-default:"60s"`
}

func MustLoad() *Config {

	var cfg Config

	if err := cleanenv.ReadEnv(&cfg); err != nil {
		log.Fatalf("Cannot read env: %s", err)
	}

	return &cfg
}
