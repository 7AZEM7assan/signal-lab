# Signal Lab task runner. Run `make help`.
SHELL := /bin/bash
SIM   := PYTHONPATH=sim python3 -m signallab_sim
URL   ?= http://localhost:8088
TEST_DB_URL := postgres://signallab:signallab@127.0.0.1:55432/signallab_test?sslmode=disable
TEST_COMPOSE := docker compose -f deploy/compose.test.yml

# Load exercise parameters (override on the command line).
LOAD_DEVICES     ?= 20
LOAD_DURATION    ?= 2500
LOAD_CONCURRENCY ?= 8
LOAD_BATCH       ?= 100

.DEFAULT_GOAL := help
.PHONY: help up down reset logs migrate psql replay-sample replay-faults fmt lint build \
        test test-go test-race test-integration test-py test-sil test-all load ci ci-fast install-hooks

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

up: ## build and start the stack (app, postgres, nginx, prometheus)
	docker compose up -d --build --wait
	@echo "Monitor: $(URL)   Prometheus: http://localhost:9090"

down: ## stop the stack, keep the database volume
	docker compose down

reset: ## stop the stack and DELETE the local database volume
	docker compose down -v

logs: ## follow service logs
	docker compose logs -f app

migrate: ## apply database migrations (the app also does this on start)
	docker compose run --rm app migrate

psql: ## open psql in the database container
	docker compose exec postgres psql -U signallab -d signallab

replay-sample: ## replay the 30-event sample dataset
	$(SIM) replay data/sample.jsonl --url $(URL)

replay-faults: ## replay the sample with seeded malformed/duplicate/late faults
	$(SIM) replay data/sample.jsonl --url $(URL) --batch-size 10 --seed 7 \
	  --malformed-rate 0.1 --duplicate-rate 0.1 --late-rate 0.1

fmt: ## format Go and Python
	gofmt -w cmd internal
	cd sim && ruff format .

lint: ## gofmt check, go vet, ruff
	@test -z "$$(gofmt -l cmd internal)" || (echo "gofmt needed:"; gofmt -l cmd internal; exit 1)
	go vet ./...
	cd sim && ruff check . && ruff format --check .

build: ## build the Go binary into bin/
	go build -trimpath -o bin/signallab ./cmd/signallab

test: test-go test-py ## fast tests with no external dependencies

test-go: ## Go unit tests (database integration tests are skipped)
	go test -count=1 ./...

test-race: ## Go unit tests under the race detector
	go test -race -count=1 ./...

test-integration: ## Go tests incl. PostgreSQL integration, under the race detector
	$(TEST_COMPOSE) up -d --wait postgres
	SIGNALLAB_TEST_DATABASE_URL='$(TEST_DB_URL)' SIGNALLAB_REQUIRE_DB=1 go test -race -count=1 ./...; \
	  rc=$$?; $(TEST_COMPOSE) down -v; exit $$rc

test-py: ## Python unit tests (no Docker)
	cd sim && python3 -m pytest -m "not sil" -q

test-sil: ## software-in-the-loop tests (needs Docker; builds and starts a disposable stack)
	cd sim && python3 -m pytest -m sil -v

test-all: lint test-integration test-py test-sil ## everything

ci: ## everything the CI workflow runs, locally, with a summary (needs Docker)
	scripts/ci.sh

ci-fast: ## the same without Docker: lint, build, Go unit tests, ruff, Python unit tests
	scripts/ci.sh --fast

install-hooks: ## opt in to a pre-push hook that runs `make ci-fast` on pushes touching this project
	git config core.hooksPath "$$(git rev-parse --show-prefix)scripts/git-hooks"
	@echo "pre-push hook installed (core.hooksPath replaces any other hooks). Skip once with SKIP_CI=1 git push."

load: ## reproducible load exercise against a running stack (see README "Load exercise")
	$(SIM) generate --out data/load.jsonl --seed 1 --devices $(LOAD_DEVICES) --duration $(LOAD_DURATION) --interval 1
	$(SIM) replay data/load.jsonl --url $(URL) --batch-size $(LOAD_BATCH) --concurrency $(LOAD_CONCURRENCY) --retries 5
