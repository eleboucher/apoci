.PHONY: build test lint clean docker lint-fix fmt tidy up down e2e

BINARY := apoci
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION)"

build:
	CGO_ENABLED=1 go build $(LDFLAGS) -trimpath -o bin/$(BINARY) ./cmd/apoci

test:
	CGO_ENABLED=1 go test -race -count=1 -timeout 60s ./...

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

fmt:
	golangci-lint fmt ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/

docker:
	docker build --build-arg VERSION=$(VERSION) -t apoci:$(VERSION) .

up:
	docker compose up --build -d

down:
	docker compose down

e2e:
	docker compose -f docker-compose.e2e.yml down -v
	docker compose -f docker-compose.e2e.yml up --build --abort-on-container-exit --exit-code-from e2e
	docker compose -f docker-compose.e2e.yml down -v
