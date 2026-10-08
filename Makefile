.PHONY: build test test-integration run migrate db

build:
	go build -o bin/xiaoju ./cmd/xiaoju

test:
	go test ./...

# 需要可用的 PostgreSQL，例如 make db 启动的实例
test-integration:
	XIAOJU_TEST_DATABASE_URL=postgres://xiaoju:xiaoju@localhost:5432/xiaoju?sslmode=disable go test ./...

db:
	docker compose up -d postgres

migrate: build
	set -a && . ./.env && set +a && ./bin/xiaoju migrate up

run: build
	set -a && . ./.env && set +a && ./bin/xiaoju serve
