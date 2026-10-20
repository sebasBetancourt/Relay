.PHONY: build migrate-up migrate-down tenant

# Load DATABASE_URL from .env when present (host port is 5435 in docker-compose).
ifneq (,$(wildcard .env))
include .env
export
endif

DATABASE_URL ?= postgres://relay:relay@localhost:5435/relay?sslmode=disable

# Compile both binaries into bin/ (gitignored): bin/relay and bin/relayctl.
build:
	go build -o bin/relay ./cmd/api
	go build -o bin/relayctl ./cmd/relayctl

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
