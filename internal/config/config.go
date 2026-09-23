package config

import (
	"fmt"
	"net/http"

	"github.com/bfi-finance/lora-process-sdk/framework/external"
	"github.com/caarlos0/env/v11"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func LoadFromEnv() *Env {
	var cfg Env
	if err := env.Parse(&cfg); err != nil {
		panic(fmt.Sprintf("config: failed to load env: %v", err))
	}
	return &cfg
}

//nolint:govet,tagalign
type Env struct {
	AppName string `env:"APP_NAME" envDefault:"lpw-playground"`
	AppEnv  string `env:"APP_ENV" envDefault:"local"`

	LoggerLevel string `env:"LOGGER_LEVEL" envDefault:"debug"`

	LORASchemaBaseServerURL string `env:"LORA_SCHEMA_BASE_SERVER_URL" envDefault:"http://localhost:8080"`
	LORASchemaBasePath      string `env:"LORA_SCHEMA_BASE_PATH" envDefault:""`

	LORAGatewayBaseServerURL string `env:"LORA_GATEWAY_BASE_SERVER_URL" envDefault:"http://localhost:8081"`
	LORAGatewayAPISecret     string `env:"LORA_GATEWAY_API_SECRET,unset"`

	TemporalHost      string `env:"TEMPORAL_HOST" envDefault:"localhost"`
	TemporalPort      int    `env:"TEMPORAL_PORT" envDefault:"7233"`
	TemporalNamespace string `env:"TEMPORAL_NAMESPACE" envDefault:""`
	TemporalAPIKey    string `env:"TEMPORAL_API_KEY,unset"`

	ArangoDBURL      string `env:"ARANGODB_URL" envDefault:"http://localhost:8529"`
	ArangoDBUserName string `env:"ARANGODB_USER_NAME" envDefault:"root"`
	ArangoDBPassword string `env:"ARANGODB_PASSWORD,unset" envDefault:"pass"`
	ArangoDBDatabase string `env:"ARANGODB_DATABASE" envDefault:"worker_playground"`
}

func (e *Env) Logger() zerolog.Logger {
	level, err := zerolog.ParseLevel(e.LoggerLevel)
	if err != nil {
		level = zerolog.DebugLevel
	}
	return log.Level(level)
}

func (e *Env) HTTPClient() *http.Client {
	return &http.Client{}
}

func (e *Env) LoraConfig() external.LoraConfig {
	return external.LoraConfig{
		SchemaBaseServerURL: e.LORASchemaBaseServerURL,
		SchemaBasePath:      e.LORASchemaBasePath,

		GatewayBaseServerURL: e.LORAGatewayBaseServerURL,
		GatewayHTTPAPISecret: e.LORAGatewayAPISecret,

		TemporalHost:      e.TemporalHost,
		TemporalPort:      e.TemporalPort,
		TemporalNamespace: e.TemporalNamespace,
		TemporalAPIKey:    e.TemporalAPIKey,

		ArangoDBURL:      e.ArangoDBURL,
		ArangoDBUserName: e.ArangoDBUserName,
		ArangoDBPassword: e.ArangoDBPassword,
		ArangoDBDatabase: e.ArangoDBDatabase,
	}
}
