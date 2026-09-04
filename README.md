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

`zae discover --url …` prints what an instance declares.

## Honest status

| | |
|---|---|
| `zae doctor` (outside-in static checks) | ✅ works today |
| `zae discover` | ✅ works — and reports honestly when an instance has no discovery endpoint yet |
| Instance-side capability discovery (`/api/portal/cli/discovery`) | ✅ served by portal-api; acquire is the first service declaring itself (10 commands) |
| `zae login` (OIDC device flow) + role-gated commands | 🧭 next |
| Registered checks, `events tail`, journey smoke tests, `addon lint` | 🧭 after discovery lands |

The platform-side design lives in the zaentrum docs:
[Extending zaentrum](https://github.com/zaentrum/zaentrum/wiki/extending).

## License

[MPL-2.0](./LICENSE), like the rest of the platform.
