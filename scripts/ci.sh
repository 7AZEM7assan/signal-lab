#!/usr/bin/env bash
# Run everything the CI workflow (.github/workflows/) runs, locally, and print a summary.
#
#   scripts/ci.sh            full run: lint, Go race tests with PostgreSQL, Python tests,
#                            container build, SIL suite (needs Docker)
#   scripts/ci.sh --fast     no Docker: lint, build, Go unit tests (database integration tests
#                            are skipped), ruff, Python unit tests
#   scripts/ci.sh --no-sil   full run without the (slowest) SIL suite
#
# Exit status is non-zero if any stage fails. A missing tool is a failure, not a silent skip:
# only the stages you explicitly turned off with --fast / --no-sil are reported as SKIPPED.
#
# Environment:
#   PYTHON                 interpreter that has pytest and ruff installed (default: python3)
#   CI_DOCKER_BUILD_ARGS   extra arguments for `docker build` (e.g. "--network host")
set -uo pipefail
cd "$(dirname "$0")/.."

FAST=0
SIL=1
for arg in "$@"; do
  case "$arg" in
    --fast) FAST=1 ;;
    --no-sil) SIL=0 ;;
    -h | --help) sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $arg (try --help)" >&2; exit 2 ;;
  esac
done

PY="${PYTHON:-python3}"
TEST_COMPOSE=(docker compose -f deploy/compose.test.yml)
TEST_DB_URL='postgres://signallab:signallab@127.0.0.1:55432/signallab_test?sslmode=disable'

NAMES=()
RESULTS=()
TIMES=()
record() { NAMES+=("$1"); RESULTS+=("$2"); TIMES+=("$3"); }

run() {
  local name="$1"
  shift
  printf '\n==> %s\n' "$name"
  local start=$SECONDS
  if "$@"; then record "$name" PASS $((SECONDS - start)); else record "$name" FAIL $((SECONDS - start)); fi
}

skip() { printf '\n==> %s: skipped (%s)\n' "$1" "$2"; record "$1" "SKIPPED" 0; }

need() { # need <what> <command...>: fail loudly with an install hint when a prerequisite is missing
  local what="$1"
  shift
  if ! "$@" >/dev/null 2>&1; then echo "missing prerequisite: $what" >&2; return 1; fi
}

# ---- stage bodies ----
gofmt_check() {
  local bad
  bad="$(gofmt -l cmd internal)"
  if [ -n "$bad" ]; then echo "gofmt needed on:"; echo "$bad"; return 1; fi
}

go_tests_unit() { go test -count=1 ./...; }

go_tests_with_db() {
  need "docker (for the throwaway PostgreSQL)" docker info || return 1
  "${TEST_COMPOSE[@]}" up -d --wait postgres || return 1
  SIGNALLAB_TEST_DATABASE_URL="$TEST_DB_URL" SIGNALLAB_REQUIRE_DB=1 go test -race -count=1 ./...
  local rc=$?
  "${TEST_COMPOSE[@]}" down -v >/dev/null 2>&1
  return $rc
}

ruff_checks() {
  local ruff=(ruff)
  command -v ruff >/dev/null 2>&1 || ruff=("$PY" -m ruff)
  need "ruff (pip install -e 'sim[dev]', or set PYTHON)" "${ruff[@]}" --version || return 1
  (cd sim && "${ruff[@]}" check . && "${ruff[@]}" format --check .)
}

pytest_unit() {
  need "pytest (pip install -e 'sim[dev]', or set PYTHON)" "$PY" -m pytest --version || return 1
  (cd sim && "$PY" -m pytest -m "not sil" -q)
}

container_build() {
  need "docker" docker info || return 1
  # shellcheck disable=SC2086  # CI_DOCKER_BUILD_ARGS is intentionally word-split
  docker build ${CI_DOCKER_BUILD_ARGS:-} -t signallab:ci .
}

pytest_sil() {
  need "pytest and websockets (pip install -e 'sim[dev]', or set PYTHON)" "$PY" -c "import pytest, websockets" || return 1
  need "docker" docker info || return 1
  (cd sim && "$PY" -m pytest -m sil -v)
}

# ---- pipeline ----
run "gofmt" gofmt_check
run "go vet" go vet ./...
run "go build" go build ./...
if [ "$FAST" = 1 ]; then
  run "go test (unit; database integration tests are skipped in --fast)" go_tests_unit
else
  run "go test -race (with PostgreSQL)" go_tests_with_db
fi
run "ruff (check + format)" ruff_checks
run "pytest (unit)" pytest_unit
if [ "$FAST" = 1 ]; then
  skip "container build" "--fast"
  skip "SIL suite" "--fast"
else
  run "container build" container_build
  if [ "$SIL" = 1 ]; then run "SIL suite" pytest_sil; else skip "SIL suite" "--no-sil"; fi
fi

# ---- summary ----
printf '\n%s\n' "================ summary ================"
failed=0
for i in "${!NAMES[@]}"; do
  printf '  %-8s %-62s %4ss\n' "${RESULTS[$i]}" "${NAMES[$i]}" "${TIMES[$i]}"
  [ "${RESULTS[$i]}" = FAIL ] && failed=$((failed + 1))
done
echo "========================================="
if [ "$failed" -gt 0 ]; then echo "CI FAILED: $failed stage(s) failed"; exit 1; fi
echo "CI PASSED"
