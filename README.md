# zae — the zaentrum CLI

`zae` talks to a [zaentrum](https://github.com/zaentrum/zaentrum) instance from
where you stand: it validates an installation outside-in, and it grows its
command surface **from the instance itself**.

```
$ zae doctor --url https://media.example.org

  ✓ tls                          certificate valid, 52 days left
  ✓ routes                       3 public paths answer
  ✓ oidc issuer                  …/auth/realms/zaentrum serves discovery (advertised by /api/config)
  ✓ image registry               ghcr.io/zaentrum/portal-api:latest pulls anonymously
  ✓ capability discovery         1 service(s) declare capabilities (schema v1)

doctor: no failures

$ zae discover --url https://media.example.org
capability schema v1 · 1 service(s)

acquire (addon)
  zae acquire wanted             list requests and their state
  zae acquire missing            the backlog: monitored, aired, still wanted
  …
```

## Install

**Script** (inspect it first — it verifies checksums):

```sh
curl -fsSL https://raw.githubusercontent.com/zaentrum/zae/main/install.sh | sh
```

**Go:**

```sh
go install github.com/zaentrum/zae@latest
```

**Container** (nothing to install; handy in CI):

```sh
docker run --rm ghcr.io/zaentrum/zae doctor --url https://media.example.org
```

Binaries for linux/macOS/windows (amd64/arm64) are on the
[releases page](https://github.com/zaentrum/zae/releases). Homebrew tap and a
krew manifest are planned once the surface settles.

## The design: static core, discovered surface

The binary splits in two, and the split is the point:

- **Static core** — what must work when the platform cannot speak for itself:
  `doctor` (TLS, published routes, the OIDC **issuer trap** — the classic
  self-host boot failure, probed from outside, which is the only place it is
  visible — and anonymous registry pullability). Each check ends in a `fix:`
  line; a diagnostic that only says "degraded" makes you do the diagnosis.
- **Discovered surface** — services and addons declare commands, checks and
  topics in capability descriptors; the instance aggregates them and `zae`
  renders them at runtime. Installing an addon extends the CLI; uninstalling
  it leaves no trace; this binary compiles in **no** service names.
  Descriptors are **data, never code** — nothing an instance serves can
  execute in your terminal; the worst a descriptor can do is describe an HTTP
  call zae then makes with your own token against the instance's own APIs.

`zae discover --url …` prints what an instance declares, and
`zae <service> <command> --url …` runs one (`--arg name=value` fills path
placeholders, `--query k=v` adds parameters, `--data '{…}'` sends a body).

## Exit codes — the scripting contract

A command can vanish between two runs of the same script because the
**instance** changed, not the binary. So the exit codes distinguish three
things a static CLI never had to: "I typed nonsense", "this instance
definitively does not offer that", and "I could not find out" — because the
last two demand opposite reactions. They are stable and part of the contract.

| exit | meaning | a script should |
|---|---|---|
| `0` | ran | — |
| `1` | ran; the instance returned an error (its message is on stderr) | handle it |
| `2` | usage — malformed invocation, missing `--url`, a placeholder not supplied | fix the script |
| `3` | **not offered** — discovery answered and this instance declares no such service or command | branch: the addon is absent or the command was renamed |
| `4` | **undetermined** — discovery unreachable, or the instance predates it | retry / alert; **never** conclude the command is gone |
| `5` | declared, but the instance refused (401/403) | authenticate — see below |
| `6` | the instance speaks a newer capability schema than this binary | upgrade `zae` |

Messages name the instance and say what *is* there:

```
zae: not offered: service "acquire" on https://… declares no command "wantd"
     (it declares: wanted, request, grab, autograb, discover, …)
zae: undetermined: cannot reach capability discovery on https://… (dial tcp …: connection refused)
     — not concluding the command is gone
```

**Assert before you act.** `zae require acquire.wanted --url …` exits `0`/`3`/`4`/`6`
with nothing on stdout, so a script checks its prerequisites up front instead
of failing halfway through. A 404 while *executing* re-discovers once and
reclassifies (`3` if the command is now gone), so a stale view never surfaces
as an inexplicable server error.

**Authentication today** is a stopgap, stated as one: set `ZAE_TOKEN` to a
bearer minted elsewhere (a service account's client-credentials token, for
instance) and `zae` sends it. `zae login` (device flow) replaces this.

## Honest status

| | |
|---|---|
| `zae doctor` (outside-in static checks) | ✅ works today |
| `zae discover` | ✅ works — and reports honestly when an instance has no discovery endpoint yet |
| Instance-side capability discovery (`/api/portal/cli/discovery`) | ✅ served by portal-api; acquire is the first service declaring itself (10 commands) |
| Running discovered commands, with the exit-code contract and `zae require` | ✅ v0.2 — `ZAE_TOKEN` for auth until login lands |
| `zae login` (OIDC device flow) | 🧭 next |
| Registered checks, `events tail`, journey smoke tests, `addon lint` | 🧭 next, alongside login — discovery is in place |

The platform-side design lives in the zaentrum docs:
[Extending zaentrum](https://github.com/zaentrum/zaentrum/wiki/extending).

## License

[MPL-2.0](./LICENSE), like the rest of the platform.
