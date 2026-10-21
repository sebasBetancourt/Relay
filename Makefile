.PHONY: build migrate-up migrate-down tenant

# Load DATABASE_URL from .env when present (host port is 5435 in docker-compose).
ifneq (,$(wildcard .env))
include .env
export
endif

DATABASE_URL ?= postgres://relay:relay@localhost:5435/relay?sslmode=disable

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO := github.com/sebasBetancourt/relay/internal/buildinfo
LDFLAGS := -s -w -X $(BUILDINFO).Version=$(VERSION) -X $(BUILDINFO).Commit=$(COMMIT) -X $(BUILDINFO).Date=$(DATE)

# Compile both binaries into bin/ (gitignored), stamped with the version.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/relay ./cmd/api
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/relayctl ./cmd/relayctl

migrate-up:
	migrate -path migrations -database "$(DATABASE_URL)" up

migrate-down:
	migrate -path migrations -database "$(DATABASE_URL)" down 1

# Create a tenant and print its API key once: make tenant NAME="Acme"
# The leading @ keeps make from echoing the recipe into stdout, so
# `make tenant NAME="Acme" > acme.key` saves only the key.
tenant:
	$(if $(NAME),,$(error NAME is required, e.g. make tenant NAME="Acme"))
	@go run ./cmd/relayctl tenant create --name "$(NAME)"
