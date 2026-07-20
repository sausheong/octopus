BINARY    := octopus
CMD       := ./cmd/octopus
GONOSUMDB := GONOSUMDB=github.com/sausheong/harness
GOFLAGS   := GOWORK=off
UNAME_S   := $(shell uname -s)
RELEASE_GOALS   := $(filter v%,$(MAKECMDGOALS))
RELEASE_VERSION := $(if $(VERSION),$(VERSION),$(RELEASE_GOALS))

.PHONY: all build app installer release notary-profile open-app test test-race vet tidy run clean help

ifneq ($(strip $(RELEASE_GOALS)),)
.PHONY: $(RELEASE_GOALS)
$(RELEASE_GOALS):
	@:
endif

## build: build the macOS app or the headless binary on other systems
ifeq ($(UNAME_S),Darwin)
all: app

build: app
else
all: build

build:
	$(GONOSUMDB) $(GOFLAGS) go build -o $(BINARY) $(CMD)
endif

## app: build dist/Octopus.app
app:
	./scripts/build-macos-app.sh

## installer: build, sign, notarize, and staple a macOS installer
installer:
	./scripts/build-macos-installer.sh

## release: create a versioned GitHub release and attach the notarized installer
release:
	@test -n "$(RELEASE_VERSION)" || { echo "usage: make release vX.Y.Z" >&2; exit 1; }
	@./scripts/release.sh "$(RELEASE_VERSION)"

## notary-profile: store Apple notarization credentials in Keychain (one-time setup)
notary-profile:
	@test -n "$(APPLE_ID)" || { echo "APPLE_ID is required" >&2; exit 1; }
	@test -n "$(TEAM_ID)" || { echo "TEAM_ID is required" >&2; exit 1; }
	@test -n "$(KEYCHAIN_PROFILE)" || { echo "KEYCHAIN_PROFILE is required" >&2; exit 1; }
	@xcrun notarytool store-credentials "$(KEYCHAIN_PROFILE)" --apple-id "$(APPLE_ID)" --team-id "$(TEAM_ID)"

## open-app: build and launch the macOS app
open-app: app
	open dist/Octopus.app

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

## run: build and run Octopus for the current system
ifeq ($(UNAME_S),Darwin)
run: open-app
else
run: build
	./$(BINARY)
endif

## clean: remove compiled binary
clean:
	rm -f $(BINARY)
	rm -rf dist/Octopus.app dist/Octopus.iconset
	rm -f dist/Octopus-*.pkg

## help: list available targets
help:
	@grep -E '^## ' Makefile | sed 's/## /  /'
