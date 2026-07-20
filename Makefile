BINARY    := octopus
CMD       := ./cmd/octopus
GONOSUMDB := GONOSUMDB=github.com/sausheong/harness
GOFLAGS   := GOWORK=off

.PHONY: all build test test-race vet tidy run clean help

all: build

## build: compile the binary
build:
	$(GONOSUMDB) $(GOFLAGS) go build -o $(BINARY) $(CMD)

## test: run all tests
test:
	$(GONOSUMDB) $(GOFLAGS) go test ./...

## test-race: run all tests with the race detector
test-race:
	$(GONOSUMDB) $(GOFLAGS) go test -race ./...

## vet: run go vet
vet:
	$(GONOSUMDB) $(GOFLAGS) go vet ./...

## tidy: tidy and verify go.mod / go.sum
tidy:
	$(GONOSUMDB) GOPROXY=direct $(GOFLAGS) go mod tidy
	$(GONOSUMDB) $(GOFLAGS) go mod verify

## run: build and run with config.yaml
run: build
	./$(BINARY)

## clean: remove compiled binary
clean:
	rm -f $(BINARY)

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
