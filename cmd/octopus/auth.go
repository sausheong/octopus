package main

import (
	"fmt"

	"github.com/sausheong/octopus/config"
)

func validateRuntimeAuth(cfg *config.Config) error {
	if cfg.AuthTokenMisconfigured() {
		return fmt.Errorf("server.auth_token_env %q is configured but unset", cfg.AuthTokenEnv)
	}
	return nil
}
