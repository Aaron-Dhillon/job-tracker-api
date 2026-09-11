SHELL := /bin/bash

# Pin the toolchain: the floor lives in go.mod (currently 1.25.11, set by
# golang-migrate), and letting Go auto-download a newer toolchain would silently
# diverge from the Docker builder. Do not restate the version here -- CI reads it
# from go.mod via go-version-file, and a second copy is a second thing to drift.
export GOTOOLCHAIN := local

DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/jobtracker?sslmode=disable
export DATABASE_URL

STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.1
IMAGE := job-tracker-api

.PHONY: help tidy fmt vet lint build run dev migrate seed-admin db-up db-down db-reset psql test test-integration docker-build

help:
	@echo "db-up            start postgres and wait for it to accept connections"
	@echo "db-down          stop postgres (keeps the volume)"
	@echo "db-reset         stop postgres and delete the volume"
	@echo "migrate          apply migrations against DATABASE_URL"
	@echo "dev              run the api on the host"
	@echo "run              build and start api + postgres in docker compose"
	@echo "seed-admin       upsert the admin from ADMIN_EMAIL/ADMIN_PASSWORD"
	@echo "test             unit tests"
	@echo "test-integration unit + integration tests (needs db-up)"
	@echo "lint             go vet + staticcheck"
	@echo "build            build ./bin/api"
	@echo "docker-build     build the production image"
	@echo "psql             psql shell inside the postgres container"

tidy:
	go mod tidy

fmt:
	go fmt ./...

# -tags=integration on both: without it neither tool reads a file behind that
# build tag, which is every DB-backed test in the repo.
vet:
	go vet -tags=integration ./...

lint: vet
	go run $(STATICCHECK) -tags=integration ./...

build:
	go build -trimpath -o bin/api ./cmd/api

dev:
	go run ./cmd/api

run:
	docker compose up --build

seed-admin:
	go run ./cmd/api -seed-admin

migrate:
	go run ./cmd/api -migrate-only

db-up:
	docker compose up -d postgres
	@printf 'waiting for postgres'
	@for i in $$(seq 1 30); do \
		if docker compose exec -T postgres pg_isready -U postgres -d jobtracker >/dev/null 2>&1; then \
			echo " ready"; exit 0; \
		fi; \
		printf '.'; sleep 1; \
	done; \
	echo " timed out"; exit 1

db-down:
	docker compose down

db-reset:
	docker compose down -v

psql:
	docker compose exec postgres psql -U postgres -d jobtracker

test:
	go test -count=1 ./...

test-integration:
	go test -tags=integration -count=1 ./...

docker-build:
	docker build -t $(IMAGE) .
	@docker images $(IMAGE) --format 'image size: {{.Size}}'
