# Contributing to Signal Lab

Thanks for taking a look. Signal Lab is a small, educational reference project, so the bar for a
change is "does it keep the project small, readable and honest about what it does".

## Before you start

- For anything beyond a small fix, open an issue first so we can agree on the approach. The
  [non-goals in the README](README.md#scope-and-non-goals) (authentication, Kubernetes, cloud
  deployment, real hardware) are deliberate; changes that add them are unlikely to be accepted.
- Everything in this repository is simulated. Please do not add claims about real plants or hardware,
  or performance numbers that were not measured and recorded (see `docs/BENCHMARKS.md` for the format).

## Setting up

Prerequisites: Go 1.25, Python 3.11+, GNU make, and Docker with the Compose plugin (only for the
integration, SIL and image-build stages).

```bash
pip install -e "sim[dev]"     # pytest, websockets, ruff
make install-hooks            # optional: run `make ci-fast` before every push
```

The desktop app (`desktop/`) additionally needs Node.js 22+: `make desktop-dev` runs it from source
and `make desktop-smoke` is its end-to-end test (`xvfb-run -a make desktop-smoke` on headless Linux).
Changes to `internal/appctl`, `internal/lab`, `internal/sqlitestore` or the panel in `internal/appui`
should keep `make ci-fast` green and, when they touch what the window shows, pass the smoke test.

## Checking your change

```bash
make ci-fast   # no Docker: gofmt, go vet, go build, Go unit tests, ruff, Python unit tests
make ci        # everything the CI workflow runs, including the database tests and the SIL suite
```

Please run `make ci` (or at least `make ci-fast`) and say in the pull request what you ran. The
GitHub Actions workflow runs the same stages. A failing test is a real failure: do not skip or
disable tests to get green.

## Style

- Go: `gofmt`, `go vet`, standard library first; new dependencies need a reason in the pull request.
- Python: `ruff check` and `ruff format` (line length 110). The simulator stays standard-library only;
  `pytest`, `websockets` and `ruff` are dev-only.
- Behaviour changes need a test, and documented behaviour (README, `docs/openapi.yaml`) needs to
  match the code.

## Pull requests

Keep them focused, describe what changed and why, and include how you verified it. By contributing
you agree that your contribution is licensed under the [MIT License](LICENSE).
