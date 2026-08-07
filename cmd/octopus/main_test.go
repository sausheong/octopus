package main

import (
	"testing"

	"github.com/sausheong/octopus/config"
)

func TestValidateRuntimeAuthFailsClosed(t *testing.T) {
	t.Setenv("OCTOPUS_TEST_AUTH", "")
	if err := validateRuntimeAuth(&config.Config{AuthTokenEnv: "OCTOPUS_TEST_AUTH"}); err == nil {
		t.Fatal("configured missing auth token was accepted")
	}
	if err := validateRuntimeAuth(&config.Config{}); err != nil {
		t.Fatalf("unconfigured auth rejected: %v", err)
	}
	t.Setenv("OCTOPUS_TEST_AUTH", "secret")
	if err := validateRuntimeAuth(&config.Config{AuthTokenEnv: "OCTOPUS_TEST_AUTH"}); err != nil {
		t.Fatalf("configured auth rejected: %v", err)
	}
}
