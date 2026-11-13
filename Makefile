GO ?= go
BIN_DIR ?= bin
TEST_VIDEO ?= testdata/test-short.mp4

.PHONY: all fmt fmt-check vet test race build ci clean test-video

all: ci

fmt:
	gofmt -l -w .

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "unformatted files:"; echo "$$unformatted"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/api ./cmd/api
	$(GO) build -o $(BIN_DIR)/worker ./cmd/worker
	$(GO) build -o $(BIN_DIR)/cli ./cmd/cli

ci: fmt-check vet test race

# Builds the reproducible test clip used by the YouTube integration check.
test-video:
	./scripts/make-test-video.sh $(TEST_VIDEO)

clean:
	rm -rf $(BIN_DIR)
