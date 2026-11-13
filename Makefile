GO ?= go
BIN_DIR ?= bin
TEST_VIDEO ?= testdata/test-short.mp4
COMPOSE_FILE ?= deployments/docker-compose.yml
COMPOSE_PROJECT ?= it-news-shorts
COMPOSE ?= docker compose -p $(COMPOSE_PROJECT) -f $(COMPOSE_FILE)

POSTGRES_DSN ?= postgres://it_news_app:it_news_local_dev@127.0.0.1:5433/it_news?sslmode=disable
MINIO_ENDPOINT ?= 127.0.0.1:9000
MINIO_ACCESS_KEY ?= minioadmin_dev
MINIO_SECRET_KEY ?= minioadmin_dev_pass
MINIO_BUCKET ?= it-news-media
TEMPORAL_ADDRESS ?= 127.0.0.1:7233
TEMPORAL_NAMESPACE ?= it-news
TEMPORAL_TASK_QUEUE ?= IT_NEWS_QUEUE

export POSTGRES_DSN MINIO_ENDPOINT MINIO_ACCESS_KEY MINIO_SECRET_KEY MINIO_BUCKET TEMPORAL_ADDRESS TEMPORAL_NAMESPACE TEMPORAL_TASK_QUEUE

.PHONY: all fmt fmt-check vet test test-unit test-race test-integration race build ci clean test-video \
	infra-up infra-down infra-destroy-volumes infra-logs infra-status compose-check \
	migrate-up migrate-down smoke-test

all: ci

fmt:
	gofmt -l -w .

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "unformatted files:"; echo "$$unformatted"; exit 1; fi

vet:
	$(GO) vet ./...

test: test-unit

test-unit:
	$(GO) test -count=1 ./...

test-race: race

race:
	$(GO) test -race -count=1 ./...

test-integration:
	$(GO) test -tags=integration -v -count=1 ./internal/postgres ./internal/storage ./internal/workflows

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/api ./cmd/api
	$(GO) build -o $(BIN_DIR)/worker ./cmd/worker
	$(GO) build -o $(BIN_DIR)/cli ./cmd/cli

ci: fmt-check vet test-unit test-race

compose-check:
	$(COMPOSE) config --quiet
	@POSTGRES_ADMIN_USER=u POSTGRES_ADMIN_PASSWORD=p POSTGRES_DB=d POSTGRES_USER=u POSTGRES_PASSWORD=p \
	TEMPORAL_DB_USER=t TEMPORAL_DB_PASSWORD=p MINIO_ACCESS_KEY=k MINIO_SECRET_KEY=s MINIO_BUCKET=b \
	POSTGRES_HOST=db.example.internal MINIO_ENDPOINT=s3.example.internal:9000 TEMPORAL_ADDRESS=temporal.example.internal:7233 \
	docker compose -f $(COMPOSE_FILE) -f deployments/docker-compose.prod.yml config --quiet

infra-up:
	$(COMPOSE) up -d --build

infra-down:
	$(COMPOSE) down --remove-orphans

infra-destroy-volumes:
	@echo "DANGER: removing persistent volumes for $(COMPOSE_PROJECT)..."
	$(COMPOSE) down -v --remove-orphans

infra-logs:
	$(COMPOSE) logs -f --tail=100

infra-status:
	$(COMPOSE) ps
	$(GO) run ./cmd/cli infra status

migrate-up:
	$(GO) run ./cmd/cli infra migrate-up

migrate-down:
	$(GO) run ./cmd/cli infra migrate-down

smoke-test:
	$(GO) run ./cmd/cli infra smoke-test

# Builds the reproducible test clip used by the YouTube integration check.
test-video:
	./scripts/make-test-video.sh $(TEST_VIDEO)

clean:
	rm -rf $(BIN_DIR)
