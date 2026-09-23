package main

import (
	"lora-process-worker-playground/internal"
	"lora-process-worker-playground/internal/config"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog/log"
)

func main() {
	_ = godotenv.Load()
	cfg := config.LoadFromEnv()
	logger := cfg.Logger()

	lc := cfg.LoraConfig()
	lc.Logger = &logger

	logger.Info().Str("app", cfg.AppName).Str("env", cfg.AppEnv).Msg("starting playground worker...")

	if err := internal.RunLoraWorker(lc, cfg.HTTPClient()); err != nil {
		log.Fatal().Err(err).Msg("worker exited with error")
	}
}
