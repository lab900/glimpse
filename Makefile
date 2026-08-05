DB_PATH ?= ./data/glimpse.db

.PHONY: generate migrate-up migrate-down run backup build

build: generate
	go build -o glimpse ./cmd/server

generate:
	sqlc generate

migrate-up:
	goose -dir sql/migrations sqlite3 "$(DB_PATH)" up

migrate-down:
	goose -dir sql/migrations sqlite3 "$(DB_PATH)" down

run: generate
	go run ./cmd/server

backup:
	@echo "Backing up $(DB_PATH) ..."
	cp "$(DB_PATH)" "$(DB_PATH).backup-$$(date +%Y%m%d-%H%M%S)"
	@echo "Done."
