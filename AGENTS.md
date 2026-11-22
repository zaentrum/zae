# zae

zae is the platform's command-line client, built on Go's standard library alone. It
checks an instance from outside (`zae doctor`; with `--sign-in` it also signs in as a
person does and uses the platform, reading only), signs in with the OAuth device grant
(`zae login`), and drives the platform through portal-api's admin API: addons
(`zae addon`), the platform's updates (`zae platform`), the first-run checklist
(`zae setup`) and the debug console (`zae debug`). The commands that services and addons
declare it reads from the instance (`zae discover`, `zae <service> <command>`) and
compiles in none. People and scripts run it against an instance; the operator runs its
image, `ghcr.io/zaentrum/zae`, as the Job that verifies the platform after every update.
How a change is proposed and what a pull request has to pass is in the
[guide to contributing](https://github.com/zaentrum/.github/blob/main/CONTRIBUTING.md);
what may be built is in [building from the specification](https://github.com/zaentrum/zaentrum/blob/main/AGENTS.md).

## Build

```sh
go build ./...
```

Go 1.24 (`go.mod`), no dependencies. On a tag `v*`, `release.yml` publishes a release
with GoReleaser; started by hand (`workflow_dispatch`, input `tag`), it builds that tag's
source with the steps on `main` and publishes it the same way (`.goreleaser.yaml`:
linux, darwin and windows on amd64 and arm64, the version stamped into `main.version`,
archives and `checksums.txt`). The same six
binaries, without publishing, go into `dist/`, which no `.gitignore` covers; GoReleaser
needs a newer Go than zae, which the go command fetches by itself:

```sh
go run github.com/goreleaser/goreleaser/v2@latest build --snapshot --clean
```

The image is the static binary on distroless; `image.yml` builds it for linux/amd64 and
linux/arm64 on `main` (`:latest`) and on a tag `vX.Y.Z` (`:vX.Y.Z`, `:X.Y`):

```sh
docker build --build-arg VERSION=dev -t zae .
```

## Test

As `ci.yml` runs them on a pull request:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
go build ./...
```

The tests need Go alone. The instances they talk to (portal-api, the identity provider
with its login page, chino-api, the catalog console's API) are `httptest` servers in the
tests, and the stored sessions go to a temporary `XDG_CONFIG_HOME`.

## Run it

```sh
go run . doctor --url https://media.example.org
go run . discover --url https://media.example.org
```

Put the address of the reference demo
([how it is deployed](https://github.com/zaentrum/zaentrum/blob/main/docs/reference-demo.md))
or of your own instance in place of `https://media.example.org`; these two need no
account. `doctor --sign-in` signs in as a person with `ZAE_DOCTOR_USER` and
`ZAE_DOCTOR_PASSWORD`, or else uses the bearer the other commands send. `addon`,
`platform`, `setup` and `debug` need an admin's bearer: `ZAE_TOKEN` when it is set, else
the session `zae login --url …` stored in `~/.config/zae/credentials.json` (under
`$XDG_CONFIG_HOME` when that is set). There are no recorded answers outside the tests.

## What the tests leave unchecked

- A real instance: zae meets one only outside this repository, when the operator runs
  the image to verify a platform.
- `doctor`'s checks of TLS, the public routes and the registry: its tests stub them out
  (the issuer check has tests of its own).
- Opening the browser in `zae login` (`open`, `xdg-open`, `rundll32`), the terminal
  handling in `internal/term` that keeps a secret from echoing, and `main.go`.
- Other systems than Linux on a pull request: darwin and windows compile in a release's
  GoReleaser run, and the image is built on `main` and on a tag only.
- `install.sh`, which installs a release's archive after checking it against
  `checksums.txt`.

## Before a pull request

- `gofmt -w .`: `ci.yml` fails on any file gofmt would change.
- The organisation's shared check, the workflow's `neutrality` job, is the one
  neutrality check.
