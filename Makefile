DB_PATH ?= ./data/glimpse.db
VERSION ?= $(shell git describe --tags --always --dirty)
OTEL_AGENT_VERSION ?= 2.30.0

# use PATH if available, fall back to the go install location
SQLC  ?= $(or $(shell command -v sqlc),$(shell go env GOPATH)/bin/sqlc)
GOOSE ?= $(or $(shell command -v goose),$(shell go env GOPATH)/bin/goose)

.PHONY: generate migrate-up migrate-down run backup build tools test agent

tools:
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
	go install github.com/pressly/goose/v3/cmd/goose@latest

build: generate
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o glimpse ./cmd/server

test:
	go test ./...

agent:
	curl -fL -o opentelemetry-javaagent.jar https://repo1.maven.org/maven2/io/opentelemetry/javaagent/opentelemetry-javaagent/$(OTEL_AGENT_VERSION)/opentelemetry-javaagent-$(OTEL_AGENT_VERSION).jar

generate:
	$(SQLC) generate

migrate-up:
	$(GOOSE) -dir sql/migrations sqlite3 "$(DB_PATH)" up

migrate-down:
	$(GOOSE) -dir sql/migrations sqlite3 "$(DB_PATH)" down

run: generate
	go run ./cmd/server

backup:
	@echo "Backing up $(DB_PATH) ..."
	cp "$(DB_PATH)" "$(DB_PATH).backup-$$(date +%Y%m%d-%H%M%S)"
	@echo "Done."
