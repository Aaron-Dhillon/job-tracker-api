SHELL := /bin/bash

# Pin the toolchain: the floor lives in go.mod (currently 1.25.11, set by
# golang-migrate), and letting Go auto-download a newer toolchain would silently
# diverge from the Docker builder. Do not restate the version here -- CI reads it
# from go.mod via go-version-file, and a second copy is a second thing to drift.
export GOTOOLCHAIN := local

# The host-facing default, used by migrate, psql, db-up and the test suites.
# The api running under `make run` gets a different one -- inside compose the
# host is `postgres`, not localhost -- set on the service in docker-compose.yml.
DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/jobtracker?sslmode=disable
export DATABASE_URL

# dev and seed-admin run the api on the host, so they need JWT_SECRET and the
# ADMIN_* pair, which live in .env and nowhere else. Sourcing the file in the
# recipe beats asking anyone to export three variables by hand; `set -a` is what
# turns its assignments into exports, and the missing-file guard keeps a fresh
# clone working. Values in .env override the default above, which is the point:
# .env is where a URL that is not localhost gets written down.
ENV_FILE := .env
load_env = set -a; [ -f $(ENV_FILE) ] && . ./$(ENV_FILE); set +a

STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.1
IMAGE := job-tracker-api

.PHONY: help tidy fmt vet lint build run dev migrate seed-admin db-up db-down db-reset psql test test-integration docker-build

help:
	@echo "db-up            start postgres and wait for it to accept connections"
	@echo "db-down          stop postgres (keeps the volume)"
	@echo "db-reset         stop postgres and delete the volume"
	@echo "migrate          apply migrations against DATABASE_URL"
	@echo "dev              run the api on the host, loading .env"
	@echo "run              build and start api + postgres in docker compose"
	@echo "seed-admin       upsert the admin from ADMIN_EMAIL/ADMIN_PASSWORD in .env"
	@echo "test             unit tests"
	@echo "test-integration unit + integration tests (needs db-up; uses jobtracker_test)"
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

# GOTOOLCHAIN=auto for this one command. staticcheck v0.8.1 declares
# `go 1.26.0`, above go.mod's 1.25.11 floor, so under the repo-wide `local` pin
# it refuses to build on any machine running exactly the version go.mod asks
# for -- CI included, since setup-go reads go-version-file. Letting Go fetch the
# toolchain staticcheck wants is safe: the pin exists so the api binary matches
# what the Docker builder produces, and a linter is not part of that binary.
lint: vet
	GOTOOLCHAIN=auto go run $(STATICCHECK) -tags=integration ./...

build:
	go build -trimpath -o bin/api ./cmd/api

dev:
	@$(load_env); go run ./cmd/api

run:
	docker compose up --build

seed-admin:
	@$(load_env); go run ./cmd/api -seed-admin

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
