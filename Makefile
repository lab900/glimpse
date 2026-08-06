DB_PATH ?= ./data/glimpse.db

# use PATH if available, fall back to the go install location
SQLC  ?= $(or $(shell command -v sqlc),$(shell go env GOPATH)/bin/sqlc)
GOOSE ?= $(or $(shell command -v goose),$(shell go env GOPATH)/bin/goose)

.PHONY: generate migrate-up migrate-down run backup build tools

tools:
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
	go install github.com/pressly/goose/v3/cmd/goose@latest

build: generate
	go build -o glimpse ./cmd/server

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
