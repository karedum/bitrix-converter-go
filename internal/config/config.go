package config

import (
	"github.com/ilyakaznacheev/cleanenv"
	"log"
	"time"
)

type Config struct {
	Env       string `env:"ENV"`
	APIConfig APIConfig
	Rabbit    RabbitConfig
	Convert   ConvertConfig
}

type ConvertConfig struct {
	SuccessDir      string `env:"SUCCESS_DIRECTORY"`
	DownloadDir     string `env:"DOWNLOAD_DIRECTORY"`
	MaxVideoSize    int64  `env:"MAX_VIDEO_SIZE"`
	MaxDocumentSize int64  `env:"MAX_DOCUMENT_SIZE"`
}

type RabbitConfig struct {
	User         string `env:"RABBITMQ_USER" env-required:"true"`
	Password     string `env:"RABBITMQ_PASSWORD"`
	Host         string `env:"RABBITMQ_HOST" env-required:"true"`
	Port         string `env:"RABBITMQ_PORT" env-required:"true"`
	DefaultQueue string `env:"RABBITMQ_DEFAULT_QUEUE" env-required:"true"`
}

type APIConfig struct {
	Port        string        `env:"API_PORT"`
	Timeout     time.Duration `env:"API_TIMEOUT"`
	IdleTimeout time.Duration `env:"API_IDLE_TIMEOUT"`
}

func MustLoad() *Config {

	var cfg Config

	if err := cleanenv.ReadEnv(&cfg); err != nil {
		log.Fatalf("Cannot read env: %s", err)
	}

	return &cfg
}
