# zae — the zaentrum CLI

`zae` talks to a [zaentrum](https://github.com/zaentrum/zaentrum) instance from
where you stand: it validates an installation outside-in, walks a fresh one
through its first-run setup, adds addons from their Helm charts or by their
address, drives the platform's updates, reads its logs and events, and grows
its command surface **from the instance itself**.

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
  `addon` is static for the same reason: an addon cannot declare the command
  that installs it. So is `debug`: a platform's logs are read when something
  is wrong with it. And `setup`: a fresh platform has nothing installed that
  could declare a command.
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

## The platform checking itself — `zae doctor --sign-in`

The static checks stop at the front door. `--sign-in` goes one step further
in, and no further than a person does: it signs in through the instance's own
login page, as its web client does, and uses the platform with that token —
reading only.

```
$ ZAE_DOCTOR_USER=… ZAE_DOCTOR_PASSWORD=… zae doctor --url https://media.example.org --sign-in
  …the static checks…

  ✓ sign-in                      with a password through the login page, as a person does (ZAE_DOCTOR_USER, ZAE_DOCTOR_PASSWORD)
  ✓ chino-api: config            the issuer https://media.example.org/auth/realms/zaentrum and the web client "chino-web"
  ✓ sign-in: issuer              the login page is at https://media.example.org/auth/realms/zaentrum/protocol/openid-connect/auth
  ✓ sign-in: authorization       the login page answers for "chino-web", its form posting to /login-actions/authenticate
  ✓ sign-in: login form          the account signed in, and the identity provider redirected back to the web client
  ✓ sign-in: callback            back at https://media.example.org/auth/callback with a code, and the state is this request's
  ✓ sign-in: token exchange      the code was exchanged for an access token, an ID token and a refresh token
  ✓ sign-in: id token            issued by https://media.example.org/auth/realms/zaentrum for "chino-web", answering this sign-in, valid until 10:10 UTC
  ✓ sign-in: access token        issued by the issuer the instance advertises, for "chino" as chino-api requires
  ✓ portal: launchpad            4 tiles open apps at /chino/, /katalog/, /katalog-manage/
  ✓ app /portal/                 serves its page and /portal/assets/index-BGvfzfuV.js (application/javascript)
  ✓ app /chino/                  serves its page and /chino/assets/index-HxjBeITW.js (application/javascript)
  …
  ✓ chino-api: item detail       "Sintel" opens with 9 credits, each with a person, a name and a role
  ✓ chino-api: portrait          "Thom Hoffman": image/jpeg, 49 KB
  - chino-api: playback          no packaged title to play
  ✓ katalog-manager: graphql     answers GraphQL at /api/manage/query

doctor: no failures
```

This is the check a platform runs on **itself** after every update: the
operator runs the `ghcr.io/zaentrum/zae` image as a Job in the platform's own
namespace, signed in as a test account it created — so no password leaves the
cluster — and records the outcome on its resource. `zae platform status`
shows the last one, and `zae platform verify` asks for one now.

- **Credentials** come from `ZAE_DOCTOR_USER` and `ZAE_DOCTOR_PASSWORD`, never
  from flags (a flag is visible in the process list and kept in shell
  history), and neither is ever printed. Without them doctor uses the bearer
  every command sends — `ZAE_TOKEN`, else the session `zae login` stored for
  the instance — and without that the signed-in checks are one skip line that
  says how to run them. A stored session belongs to the CLI's own client,
  whose tokens chino-api may not accept: that is a skip, not a failure.
- **Signing in the way a person does.** The issuer and the web client's id
  come from the instance's `/api/config`; the authorization request carries
  PKCE S256, a state and a nonce; the login form is found by where it posts,
  not by how the login theme draws it; the redirect back to
  `<instance>/auth/callback` is where it stops; the code is exchanged, and the
  ID token's issuer, audience and nonce are checked. Each step is its own line
  with a fix that names the likely cause — the issuer trap, a redirect URI the
  client does not allow, a wrong password, a required action pending
  (`UPDATE_PASSWORD`, `VERIFY_PROFILE`, …), a second factor. The password is
  posted once, and only to the identity provider's own origin.
- **Then the platform, with the token.** The launchpad says where the apps
  are mounted, and each must serve its page *and* the bundle the page loads —
  a missing bundle behind a single-page fallback is a blank screen that a
  status code calls healthy. chino-api: a list, a title with well-formed
  credits, its poster, a person found by a credit's name, their page and
  portrait, the master playlist of a packaged title. The catalog console's
  GraphQL, where a refusal in the service's own words counts as reachable.
- **Data-independent, read-only, bounded.** An empty instance passes: a check
  that needs a title, a person or a package the instance does not have is a
  skip. Every request is a read — GETs, the sign-in's two posts, one GraphQL
  query — each under doctor's short timeout, and the token is sent only to the
  instance's own origin.
- **`--report FILE`** writes the run as one line of JSON of at most 4096
  bytes, which is what Kubernetes keeps of `/dev/termination-log`:

  ```json
  {"v":1,"zae":"v0.6.0","url":"https://media.example.org","passed":26,"failed":1,"warned":0,"skipped":0,
   "checks":[{"n":"tls","s":"ok","d":"certificate valid, 89 days left"},
             {"n":"chino-api: items","s":"fail","d":"/api/v1/items?limit=5 answered HTTP 502: … · fix: …"}]}
  ```

  When a run does not fit, the least useful text goes first — the details of
  passing checks, then of skips and warnings — and a failure is never
  dropped; the counts are always the run's own. The exit code stays doctor's:
  `1` on any failure.

## Addons from a chart — `zae addon`

An addon is a standard Helm chart; the platform's operator installs it from one
`ZaentrumAddon` resource
([ADR-0011](https://github.com/zaentrum/zaentrum/blob/main/docs/adr/0011-addon-charts-installed-by-the-operator.md)).
`zae addon` drives that through the portal's admin API — **plan first, always**:

```
$ zae login --url https://media.example.org   # or set ZAE_TOKEN to an admin bearer
$ zae addon add oci://ghcr.io/example/charts/example --version 1.2.0 \
    --url https://media.example.org --set worker.replicas=2 --set-secret database.password
database.password (secret input, not shown):

planning example from oci://ghcr.io/example/charts/example 1.2.0 on https://media.example.org …

plan for example — chart example 1.2.0 — Planned
  description An example addon
  workloads
    Deployment/example         ghcr.io/example/example:1.2.0  ports 8080
    Deployment/example-worker  ghcr.io/example/worker:1.2.0
  objects     4 (Deployment 2, Secret 1, Service 1)
  inputs
    config.key         secret, required  generated by the operator
    database.password  secret, required  set
    worker.replicas    value             2

install example (chart example 1.2.0) on https://media.example.org? [y/N] y
installing example — follow it with zae addon status example --url https://media.example.org
```

| command | does |
|---|---|
| `zae addon add <chart> --url …` | creates the addon suspended, prints the operator's plan for exactly that write, asks, installs. `<chart>` is `oci://…` with `--version` (or `:tag`), or an `https://` link to a chart archive; `--digest sha256:…` pins the archive; `--name` overrides the name taken from the reference, which zae derives exactly as the portal does |
| `zae addon add http://<service> --url …` | an addon deployed some other way, by its in-cluster address — see [below](#addons-added-by-their-address): checks what adding it does, prints it, asks, installs |
| `zae addon list --url …` | every installed addon: chart or address, the version the addon reports, a chart addon's chart version (asked for, and running when that differs), phase, ready components, and whether a refresh waits |
| `zae addon status <name> --url …` | a chart addon: phase, the chart asked for and the one running, registration, components and the current plan. An address addon: its address, version, whether a refresh waits, its containers and its setup |
| `zae addon upgrade <name> --url …` | changes the chart (`--version`, `--chart`, `--digest`), values (`--set`, `--values`) or secret inputs (the secret flags, `--clear-secret`); plans first, shows the changes, asks, installs |
| `zae addon refresh <name> --url …` | reads an address addon's manifest again after it was redeployed — checked, shown, asked |
| `zae addon remove <name> --url …` | deletes the addon and everything its chart applied; `--keep-values` keeps its values Secrets and its generated values for a later install. An address addon loses what the portal created for it; its containers keep running |

- **Nothing blocked is installed.** A plan the guardrails refused, or whose
  values fail the chart's `values.schema.json`, exits `1` — `--yes` included.
  zae waits for the plan the operator made for the generation *its own* write
  produced, never an older one; if someone else changes the addon meanwhile,
  zae stops with `1`.
- **Values.** `--values FILE` (or `-` for stdin) is one JSON object;
  `--set path=value` sets one dotted path — the value is JSON when it parses
  as JSON, otherwise a string (`--set 'tag="1.10"'` forces a string). `add`
  starts from `--values`; `upgrade` changes the current values unless
  `--values` replaces them. Values are JSON, not YAML: zae stays
  standard-library-only and does not guess YAML's types differently from Helm
  (`yq -o=json values.yaml | zae addon add … --values - --yes`).
- **Secret inputs** are stored in a Secret and never shown again; the plan says
  only *set* or *missing*:

  | flag | the value comes from |
  |---|---|
  | `--set-secret path` | a prompt on the terminal, not echoed |
  | `--set-secret-file path=FILE` | a file, one trailing newline trimmed |
  | `--secret-values FILE` or `-` | a JSON object of dotted path → string |
  | `--set-secret path=value` | the command line — visible to other local users in the process list, and kept in shell history |
  | `--secret-ref path=name[/key]` (`add`) | a key of a values Secret kept by `remove --keep-values`; the key defaults to the path |

  They add to the secret inputs already set; `upgrade --clear-secret path`
  removes one.
- **Asking.** `add`, `upgrade` and `remove` ask on stdin. Without a terminal
  there they need `--yes`; stdin cannot carry a document and an answer, or two
  documents, at once. Each of these exits `2` before anything is written.
- **Upgrades put back what they do not install.** Refused, declined, no plan in
  time, Ctrl-C or SIGTERM: zae re-reads the addon and restores the chart
  reference, version, digest, values and suspension it read before writing —
  unless someone else changed the addon since. Secret inputs cannot be put
  back, because zae never reads their values; it says which ones stay as
  written.
- **Waiting.** `--wait` follows an install until the addon is Ready *and*
  registered in the portal, and exits `1` — with the registration error, if
  there is one — if it is not after `--timeout` (default `5m`).
- Exit codes follow the contract below: `3` means no such addon, or an instance
  that cannot install addons from charts (a portal-api without the API, a
  cluster without the resource); `4` that the instance could not be reached;
  `5` a missing or refused admin bearer; `130`/`143` interrupted.

### Addons added by their address

An addon deployed some other way — its own chart, a manifest, a deployment
repository — is added by its in-cluster address: the Service of its primary
container. The portal reads the capability manifest it serves and creates what
it declares — its app, tiles, slot rows and CLI commands — under the addon's
key. It neither deploys such an addon nor deletes it.

```
$ zae addon add http://sample-addon --url https://media.example.org
check sample — Sample addon 1.2.0
  status      not installed yet
  address     http://sample-addon
  creates     1 tile, 1 slot row, 2 CLI commands, 1 container
  containers
    sample  primary  sample-addon  ready  1/1  serves the sample
install sample on https://media.example.org? [y/N] y
installed sample: 1 tile, 1 slot row, 2 CLI commands, 1 container
```

- **Checked first, always** — the console's "check": the portal answers what
  installing would do and writes nothing, zae prints it and asks, like every
  other write. `--dry-run` stops after the check, and `--json` prints the
  portal's answer to it. A portal-api older than the check installs when asked
  to check; zae says so, and a `--dry-run` that wrote exits `1`.
- **A move needs consent.** An addon installed from another address moves only
  with `--replace-address`: the portal's proxy then sends every request for
  it, with its caller's token, to the new address. Without the flag zae stops
  before writing.
- **`--space`** picks the launchpad space its tiles go to, unless the addon
  brings its own; a space the launchpad does not have is exit `3`, naming the
  ones it has. The portal keeps no record of the space an addon was added to,
  so `refresh` places its tiles in the first space unless `--space` names
  another — and says so.
- **`refresh`** reads the manifest again from the address the portal recorded,
  after the same check: `list` says `available` in its REFRESH column when the
  addon serves another manifest than the one installed. A chart addon is
  registered again by the portal itself; refreshing one is a usage error that
  names `upgrade`.
- **`status`** asks the chart API first and, when it has no addon by that name,
  reads that addon's row (`GET /api/portal/addons/{key}`, exactly as the list
  has it — a portal-api older than that read has only the list, which is read
  instead): where it was added from, the version its manifest reports, what it
  registers, its containers with their live state, and its setup — asked of
  the addon itself through the portal's app proxy, as the console does.
  `--json` prints the row.
- **`remove`** says the platform does not delete containers, and repeats what
  the portal says still runs.
- `http://` is an address — charts are fetched over https only, and a link to
  a chart archive over http is refused as one — and so is an `https://` URL
  without a path. The portal's refusals reach the terminal in its words: a
  chart addon by that name, a manifest that breaks the contract, an address
  that does not answer (`1`); its workloads unlisted for a moment (`4`).

## The platform's own updates — `zae platform`

An instance managed by the zaentrum operator declares the whole platform in one
resource: the version it is pinned to, the channel it follows, whether it
applies in-channel updates by itself. `zae platform` reads and drives that
through the portal's operator console — the same admin API the settings screen
uses — so an update does not need a terminal on the cluster.

```
$ zae platform status --url https://media.example.org
https://media.example.org — the platform
  version      1.4.0 — pinned
  channel      stable
  update mode  manual — an update is applied when someone asks for it
  phase        Ready
  running      1.4.0
  update       1.5.0 available — apply it with: zae platform update --apply --url https://media.example.org
  verified     passed 21/22, 1 skipped · 3 min ago · after the update to 1.4.0 (image set 3f9a1c0b2d4e) · job zaentrum-verify-7f3c
  host         media.example.org

NAME            GROUP          IMAGE                READY  PHASE     REASON
chino-api       platform       1.4.0                2/2    ready     -
katalog-api     platform       sha256:aaaaaaaaaaaa  0/1    degraded  ImagePullBackOff
postgres        platform       16                   1/1    ready     -
example-worker  addon:example  2.0.0                1/1    ready     -
leftover        other          latest               1/1    ready     -

the operator's controller
  version      v0.4.1
  image        ghcr.io/zaentrum/operator:v0.4.1
  installed    OLM — a subscription the cluster manages
  update       v0.5.0 available
  observed     2026-09-22T08:00:00Z
Updated outside the platform: approve the update in its OLM subscription.

$ zae platform update --apply --wait --url https://media.example.org
https://media.example.org — the platform
  version      1.4.0 → 1.5.0
  rolls        3 workloads the operator manages
apply this to https://media.example.org? [y/N] y
waiting for the platform to report 1.5.0, and every workload the operator manages to be ready (timeout 10m)
  Reconciling  1.4.0 · 2/3 ready — the operator has not reconciled this change yet (at 7, waiting for 8); waiting for 1.5.0
  Reconciling  1.5.0 · 2/3 ready — katalog-api 0/1 progressing
  Ready        1.5.0 · 3/3 ready
the platform reports 1.5.0, and every workload the operator manages is ready
```

| command | does |
|---|---|
| `zae platform status --url …` | the version the platform is pinned to — or that nothing is pinned and it follows a channel — the channel, the update mode, the phase, the version it reports running, whether an update is offered; then every workload, grouped: what the operator renders, then addons, then whatever neither claims; then the operator's own controller. Without an operator, the workloads alone ([direct mode](#direct-mode)). `--json` prints the portal's own document |
| `zae platform controller --url …` | that last section on its own, for scripts: the version in charge, its image, how it was installed, whether something newer was found — and the one line naming what updates it. `--json` prints the portal's own `controller` document, always an object |
| `zae platform update --url …` | `--version V` pins an image tag (`--version latest` follows the channel again), `--channel C` picks the release train, `--mode auto\|manual` decides whether the operator updates itself, `--apply` pins the update it has already discovered |
| `zae platform restart <workload> --url …` | rolls one workload |
| `zae platform scale <workload> <replicas> --url …` | sets one workload's replica count |
| `zae platform verify --url …` | asks the operator to verify the platform now; `--wait` follows the run this request started and prints its checks |

- **What the resource leaves unset reads as what the operator does** — the
  console's values: an unset update mode is `manual`, an unset channel
  `stable`, each marked as the default rather than shown as `-`.
- **The platform verifies itself.** After every rollout the operator runs
  `zae doctor --sign-in` in the platform's namespace as a test account it
  created, and records the result; `status` shows it on the `verified` line —
  `passed 14/14 · 3 min ago · after the update to 1.5.0 (image set …) · job …`,
  or `FAILED 2 of 14 checks` with each failing check under it, `running`,
  `error` with the operator's message, `skipped`, `never`, or `off` when
  `spec.verification.enabled` is false. The image set is the fingerprint of the
  images verified, so a run after an update to `latest` still names what it
  checked; the job is the run itself, whose log `zae debug logs <job>` reads.
- **`verify` asks, and follows its own run.** A request that is already
  waiting is joined, not doubled. `--wait` ends on the result of the run that
  answers *this* request — a run for an earlier one may finish first and does
  not count — and exits `0` when it passed, `1` when it failed or could not
  run, and `3` when the platform does not verify itself (switched off, no
  operator, or a portal-api that predates the request).

- **The controller is shown, never updated.** It runs in its own namespace,
  outside the one the portal administers, and it is installed and upgraded
  outside the product — so zae reports what is in charge and names the path for
  the source the operator declares:

  | source | what updates it |
  |---|---|
  | `olm` | approve the update in its OLM subscription |
  | `manifest` | apply the pinned install manifest, usually through the deployment repository that holds it |
  | `appliance` | update the appliance — its own update carries the controller |
  | `unknown` | whichever of the three installed it |

  What the channel serves is worded for what it is: a version reads
  `v0.5.0 available`, a moving tag reads `the "latest" channel now serves a
  different image` — an install pinned to a commit image is not "on" `latest`
  and never will be, so naming the tag as a version says nothing.

  There is no flag that does any of them. An operator that reports no
  controller — every operator older than the field — says so, and
  `zae platform controller` exits `3` (not offered by this instance) so a
  script can branch on it; a `status` is unaffected and still exits `0`.
  `zae platform` updates the platform that controller *deploys*, which is the
  other half of the job and the half that happens far more often.
- **`--apply` takes no version of its own — and names the one it read.** It
  pins the platform to what the operator discovered, so `--apply --version V`
  is a usage error (`2`), and so is `--apply --channel C`: the update on the
  shelf is the one found on the channel being followed *now*. Change the
  channel, let the operator look, then apply. The request carries the version
  zae showed you, so if the operator discovered another one in between — a
  newer release, somebody else's channel change — the platform refuses it
  (exit `1`) instead of rolling to a version nobody chose.
- **Protected workloads are refused by the platform,** in its words — the
  stateful services it keeps out of reach. zae prints that reason and exits
  `1`; it does not keep a copy of the rule.
- **Asking.** `update`, `restart` and `scale` print what will change and ask on
  stdin. Without a terminal there they need `--yes`, and exit `2` before
  anything is written.
- **Waiting follows *this* rollout,** by the rule `kubectl rollout status`
  uses: the cluster has acted on the generation the write returned, every pod
  asked for comes from the new revision, **no pod from an older revision is
  left**, and all of them are available. It prints a line per change of state
  and exits `1` on `--timeout` (default `10m`) naming what was still not ready.

  Every clause is there because a weaker one was wrong. A one-replica rollout
  surges — `maxSurge 1`, `maxUnavailable 0` — so the new pod is created *first*,
  and for the whole of its startup the cluster reports `updatedReplicas 1`,
  `readyReplicas 1`, `availableReplicas 1`: every number the size asked for,
  every one of them counting the pod from *before* the restart. Only the total
  pod count separates them — `2` during the surge, `1` once the old pod is
  gone. Against a portal-api that reports less than this, zae names the field
  it cannot see and falls back to the weaker readiness gate.
- <a id="direct-mode"></a>**Direct mode.** A portal in a cluster without an
  operator resource still manages the workloads — the console calls it direct
  mode — and restarts and scales the Deployments themselves. So does zae:
  `status` shows the workloads under the portal's note, and `restart` and
  `scale` (with `--wait`) work as they do with an operator. What only the
  operator's resource holds — `update`, `verify`, `controller` — exits `3`,
  saying it is direct mode.
- Exit codes follow the contract below: `3` means this instance does not manage
  its workloads at all (it is not running in a cluster), has no operator for
  what needs one, or runs no workload by that name.

## First-run setup — `zae setup`

A fresh platform has a first-run checklist: the launchpad shows it to an
admin until one marks setup done. `zae setup` reads the same checklist —
`GET /api/portal/setup`, admin-only, each step read live from where it is
configured: the catalog, the operator's resource, the cluster — and works
through it:

```
$ zae setup --url https://media.example.org
https://media.example.org — first-run setup · 2 of 4 steps done
  metadata     to do — no TMDB key — titles keep their file names, and get no posters or plots
               next: zae setup metadata --tmdb-key-file FILE --url https://media.example.org
                     FILE holds TMDB's API read access token (v4, it starts with eyJ)
  library      to do — no titles yet
               no scan has run yet
               copy files into the media/ folder of the media volume — the catalog reads it as /var/lib/katalog/media — then scan
               next: copy files there, then zae setup scan --url https://media.example.org
  processing   optional — the media pipeline is off — files stream as they are
               GPU: no node offers one, which the transcoder needs
               next: zae setup pipeline on --url https://media.example.org — it analyzes, transcodes and packages titles for adaptive streaming
  devices      done — phones and TVs can sign in: media.example.org answers over https
  people       manual — accounts for the people who use this server are made in the identity provider's admin console; nothing here checks them
               read: https://github.com/zaentrum/zaentrum/blob/main/docs/self-hosting.md#the-admin-console
  marked done  no — the launchpad shows this checklist to admins until it is
               next, once you are done: zae setup done --url https://media.example.org

$ zae setup metadata --tmdb-key-file ~/tmdb.token --url https://media.example.org
https://media.example.org — the catalog's TMDB key
  now          none — titles keep their file names, and get no posters or plots
  sets         the key read from the file --tmdb-key-file names, to the catalog manager's setting — it is never shown again
set the TMDB key on https://media.example.org? [y/N] y
set the TMDB key on https://media.example.org
  metadata     done — a TMDB key is set, saved just now
```

| command | does |
|---|---|
| `zae setup --url …` | the checklist: each step's state — `done`, `to do`, `scanning` or `starting`, `optional`, `manual`, or `unknown` with the portal's reason — and what to do next. People is nothing the platform can check and does not count towards done; a pipeline that is off is fine off. `--json` prints the portal's own document |
| `zae setup metadata --url … --tmdb-key-file FILE` | sets the catalog's TMDB key — TMDB's API read access token (v4) — through the portal, which hands it to the catalog manager and never stores, logs or answers it. `--tmdb-key-stdin` reads it from stdin instead: a line typed without echo on a terminal, or everything piped in |
| `zae setup scan --url …` | the catalog manager scans the library — unless a scan is under way, which it waits out as the console's button does; one that has run for half an hour has most likely stopped, and another is started |
| `zae setup pipeline on\|off --url …` | switches the media pipeline — analyzer, katalog-ingest, packager, transcoder — on the operator's resource (`PATCH /api/portal/operator {"pipeline": …}`, the operator console's write) |
| `zae setup done --url …` | marks setup done, so the launchpad stops showing the checklist |
| `zae setup reopen --url …` | shows it again |

- **The key never travels on the command line.** There is no flag that takes
  it: an argument would be kept in shell history and shown in the process
  list. An argument that may be the key is refused without being repeated —
  and so is one given as the value of a flag that takes none, and the path a
  failing `--tmdb-key-file` names, which could be the key typed in the wrong
  place. zae never prints the key: not in the summary before the question,
  not in an error, not where it quotes the portal — which never answers with
  it, and zae does not rely on that. On a terminal, `--tmdb-key-stdin` turns
  echo off, and back on however the line ends, Ctrl-C included (`130`).
- **What cannot be a key is refused before anything is sent** (`2`): empty,
  longer than the portal takes, holding whitespace or control characters —
  and TMDB's API key (v3), 32 hex digits, which the portal would take and
  TMDB refuse when the catalog signs in with it, without a word. A byte order
  mark and surrounding whitespace are trimmed.
- **Asking.** `metadata` and `pipeline` print what changes — the key in effect
  and where the new one was read from; what starts or stops, and whether a
  node offers the GPU the transcoder needs — and ask on stdin; so does `done`
  while steps are open, naming them. Without a terminal they need `--yes`, and
  exit `2` before anything is read or written; piped into
  `--tmdb-key-stdin`, stdin cannot carry the answer as well, so `--yes` is
  needed there too. `scan` and `reopen` do not ask, as the console does not.
  A pipeline already as asked, setup already done or already open: nothing is
  written, exit `0`.
- **Through the catalog manager.** The key and a scan reach the catalog
  manager through the portal, with the admin's own bearer. No catalog manager
  is `3` — said before asking; one that refuses the admin `5`; one that does
  not answer `4`, since the portal took the write and cannot say whether it
  arrived; one that answers an error `1`.
- Exit codes follow the contract below: `3` also means a portal-api older than
  the checklist, or than the pipeline switch, and an instance with no
  operator's resource to switch the pipeline on; `5` without the admin role.

## The debug console — `zae debug`

The portal's debug console reads every container's log, the platform's event
bus and a support bundle for a bug report. `zae debug` reads the same, over the
same read-only, admin-only API (`GET /api/portal/debug/…`) — no cluster
credentials, no kubectl:

```
$ zae debug logs example-worker --url https://media.example.org --tail 2
[example-worker-7d79fd4c4b-xprff/app] 2026-10-03T22:24:15.287074864Z picked up job 4211
[example-worker-7d79fd4c4b-qfmtj/app] 2026-10-03T22:24:23.819406222Z picked up job 4212
[example-worker-7d79fd4c4b-xprff/app] 2026-10-03T22:24:34.302172610Z job 4211 done in 19.0s
[example-worker-7d79fd4c4b-qfmtj/app] 2026-10-03T22:24:41.024273539Z job 4212 done in 17.2s

$ zae debug events --url https://media.example.org --topic platform.item.added
TIME                  TOPIC                TYPE   ITEM    AT
2026-10-03T12:00:01Z  platform.item.added  added  item-1  p0·7
```

| command | does |
|---|---|
| `zae debug logs <workload\|pod> --url …` | a workload's container logs — every pod, every container unless `--container C` picks one — merged by time, each line with `[pod/container]` when there is more than one. `--tail N` per container (the portal's default 500, its most 5000), `--since 10m`, `--follow`, `--json` (one object per line) |
| `zae debug events --url …` | what the portal's event tap read from the bus since portal-api started — at most the last 500 — oldest first; `--topic`, `--limit`, `--payload`, `--json` (the portal's document, newest first) |
| `zae debug bundle -o FILE --url …` | the portal's support bundle — workloads and operator state, every container's recent log, the bus topology, the registry, the non-secret settings — plus a section about this zae, to a new file (mode `0600`) or with `-o -` to stdout; `--without logs,kafka,…` leaves sections out |

- **Redacted twice.** The portal scrubs credentials out of every debug view;
  zae scrubs what it prints or writes again, with the portal's own rules, so a
  portal-api older than a fix to them — or a field it does not scrub, like an
  event's key — still does not put a credential in a terminal or a file. JSON
  is redacted value by value and stays JSON.
- **A workload's pods are found by their owner.** The portal names the
  workload that runs each pod, from its owner references — the Deployment
  behind a ReplicaSet, a StatefulSet, a DaemonSet, a Job — and a workload's
  pods are the ones it names; a pod nothing owns goes by its own name. A pod's
  own name works too, and so does a verification's job, as `status` names it.
  A portal-api older than that lists pods without their owners, and zae reads
  the names Kubernetes gives them instead — `<name>-<template hash>-<suffix>`
  for a Deployment's, `<name>-<suffix>` for a Job's or a DaemonSet's,
  `<name>-<ordinal>` for a StatefulSet's — with the generated parts in
  Kubernetes' own alphabet, which keeps `chino-api` from claiming
  `chino-api-worker`'s pods. What a name cannot tell is a Job generated from
  a workload's name: by its pods' names, `api-bcdfg` reads as the Deployment
  `api`.
- **`--follow` reads again every two seconds,** as the console's live view
  does: each read asks for the lines from the newest one printed on
  (`sinceTime`, that line's own stamp, on the cluster's clock), and prints
  only the lines newer than it — told apart by the cluster's nanosecond
  timestamps, because the cluster reads `sinceTime` to the second and answers
  the whole second again. A container that has printed nothing yet is read by
  window — the seconds since the last read, widened by five, on this
  machine's clock — and so is every container against a portal-api that
  ignores `sinceTime` or refuses it, after one note saying so. It lists the
  pods again every round, so a rollout's new pods are read from their first
  line and the retired ones are said to be gone — once, whether a read or the
  listing finds out first; a StatefulSet's pod back under its own name is read
  again. A failure that passes is said once; a refused bearer ends it with
  `5`; Ctrl-C with `130`.
- **The bundle is never written over a file,** and the portal takes up to a
  minute to assemble it, which zae waits out.
- **A read answers what is wrong with it.** A pod that went after zae listed
  it is the portal's 404, a read the cluster refuses as asked its 400, in the
  cluster's words. A pod asked for by its own name and gone is `3`, naming the
  workload whose pods run in its place; one pod of a workload gone is a note,
  and the others are printed; every pod of a workload gone by the time it is
  read is `4` — its pods are being replaced, which says nothing about whether
  it runs. A container the pod no longer runs is `3`, one still waiting to
  start `1`. Neither ends a `--follow`.
- Exit codes: `3` for what is not there, saying what is — no such workload,
  pod, container or topic, no event bus, no pods outside a cluster; `5`
  without the admin role; `4` when the instance cannot be reached; `1` when a
  container cannot be read, after printing the ones that could.

## Signing in — `zae login`

```
$ zae login --url https://media.example.org
signing in to https://media.example.org
  identity provider: https://media.example.org/auth/realms/zaentrum
  client:            zae

open https://media.example.org/auth/realms/zaentrum/device?user_code=WDJB-MJHT
  (it already carries the code; confirm that it shows WDJB-MJHT)

waiting for you to finish in the browser (Ctrl-C to stop) …

signed in to https://media.example.org
  as         ada (8b1f-…)
  admin role yes — the instance grants this bearer its admin role ("zaentrum-admin")
  expires    Mon, 21 Sep 2026 21:48:23 CEST
             renewed automatically while the session lives
  stored in  /home/ada/.config/zae/credentials.json
```

It is the **OAuth 2.0 Device Authorization Grant**
([RFC 8628](https://www.rfc-editor.org/rfc/rfc8628)) with PKCE: a CLI is a
public client that cannot keep a secret and cannot receive a browser redirect,
and the device grant is the one flow designed for exactly that. The browser
that signs in does not have to be on this machine — which is what makes this
work over ssh, in a container, or on a server with no desktop at all.

`zae` compiles in no realm, no URL and no client id. The **instance** says
which issuer it validates against and which client a CLI should use (the
`auth` object in its discovery document); the **issuer** says where its
endpoints are (`/.well-known/openid-configuration`). Nothing is built by
string concatenation, because a realm may be served under a path prefix.

| command | does |
|---|---|
| `zae login --url …` | signs in and stores the session. `--no-browser` prints the URL instead of opening one; `--issuer` and `--client-id` sign in to an instance that does not advertise them; `--scope` overrides the requested scopes |
| `zae logout --url …` | ends that instance's session at its identity provider, then forgets it; `--all` does so for every one and removes the file |
| `zae whoami --url …` | who the instance says the bearer is: its subject, username, roles, whether it grants the bearer its admin role (`--role R` adds whether it lists R), and when it stops taking it; `--json`. It never prints the token |

**Where credentials live.** `~/.config/zae/credentials.json`
(`$XDG_CONFIG_HOME/zae/credentials.json` when that is set), mode `0600` in a
`0700` directory, keyed by instance URL — a session at one instance is
worthless at another, and must never travel there. Each entry holds the access
token, the refresh token, the expiry, the issuer and the client id.

**Which bearer a command sends**, in order:

1. `ZAE_TOKEN`, when set. A script that sets it is naming the identity it
   means, and a developer's own login must not quietly override it.
2. The stored session for that instance — renewed with its refresh token when
   it has expired, and the renewal written back, so one expiry costs one extra
   round trip and not one per command.

If the renewal fails, the session ended: `zae` says *session expired — run:
zae login --url …* and exits `5` rather than sending a token it knows is dead.
A token that is real but lacks the role is a different message, because it
needs a different fix: signing in again with the same account changes nothing.

**Signing out ends the session where it lives.** `zae logout` revokes the
refresh token at the issuer — the revocation endpoint its metadata names
([RFC 7009](https://www.rfc-editor.org/rfc/rfc7009)) — and then removes the
stored entry; forgetting alone would leave a session that outlives the file it
was copied from. The entry goes whatever the issuer answers, so nobody stays
signed in because an identity provider is down, and the exit code says whether
the session ended there too: `0` it did, `3` the issuer offers no revocation,
`4` it could not be asked, `1` it refused — each with how long the session
lives on. An access token already issued stays valid until it expires, minutes
later.

**Who decides the admin role.** The instance: its admin role is a setting, and
a portal may take it only on tokens issued to its own clients. `whoami` and
`login` therefore ask (`GET /api/portal/me`) instead of reading a role name
out of the token — a token can carry the role and still be refused, and
whoami says why. The answer also names the token's subject and when the
instance stops taking it, which is all an opaque token has to go by; an
instance with its authentication switched off says that no token stands behind
the caller, and whoami shows no expiry rather than the token's own. Against a
portal-api that cannot say, they read the token as before and say that they
did; an instance that cannot be asked is exit `4`, one that refuses the bearer
`5`.

**Tokens are never printed.** Not by `login`, not by `whoami`, not in an error
message — the one rule that keeps them out of scrollback, CI logs and pasted
bug reports.

**What an operator configures** — a public client (default id `zae`) with the
device grant enabled, whose tokens carry realm roles: see
[the CLI contract](https://github.com/zaentrum/zaentrum/blob/main/docs/extending/cli.md#what-an-operator-must-configure).

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

**Authentication** is `zae login` — see below. `ZAE_TOKEN` still works and
still wins, for service accounts and CI.

## Honest status

| | |
|---|---|
| `zae doctor` (outside-in static checks) | ✅ works today |
| `zae doctor --sign-in` / `--report` (the check a platform runs on itself) | ✅ works — run against a live instance, password sign-in through its login page |
| `zae platform verify`, and the `verified` line of `status` | 🔶 built against portal-api's verification API; needs an operator that verifies the platform |
| `zae discover` | ✅ works — and reports honestly when an instance has no discovery endpoint yet |
| Instance-side capability discovery (`/api/portal/cli/discovery`) | ✅ served by portal-api; acquire is the first service declaring itself (10 commands) |
| Running discovered commands, with the exit-code contract and `zae require` | ✅ v0.2 |
| `zae addon add/list/status/upgrade/remove` (charts installed by the operator) | 🔶 built against the addon chart API; needs an instance whose portal-api and operator ship it |
| Addons added by their address: `zae addon add http://…`, `refresh`, and them in `list`/`status`/`remove` | ✅ `list` and `status` (by `GET /api/portal/addons/{key}`) run against a live instance; the writes are built against the portal's address-addon API (`POST`/`DELETE /api/portal/addons`) |
| `zae platform status/controller/update/restart/scale` (the operator console) | 🔶 built against the portal's operator console; needs an operator-managed instance — an older portal-api works, with a weaker `--wait` that says so. `controller` shows what is in charge and names where it is updated; updating it is out of scope by design |
| `zae platform` in direct mode (no operator resource) | 🔶 built against the portal's operator console, as the console behaves without an operator |
| `zae login` / `logout` / `whoami` (device grant with PKCE, refresh, per-instance sessions) | 🔶 built; needs a portal-api that advertises `auth` and an operator-created public client. `whoami` asking the instance — its subject and expiry too — runs against a live one; `logout` revoking at the issuer (RFC 7009) is built against the issuer's metadata |
| `zae debug logs` / `events` / `bundle` (the portal's debug console) | ✅ `logs` — pods by their owner, `--follow` by `sinceTime` — and `events` run against a live instance; `bundle` is built against the portal's support bundle |
| `zae setup` (the first-run checklist) | 🔶 the checklist runs against a live instance; `metadata`, `scan`, `pipeline`, `done` and `reopen` are built against the portal's setup API and the operator console's pipeline switch |
| Registered checks, `events tail`, journey smoke tests, `addon lint` | 🧭 next — discovery and login are in place |

The platform-side design lives in the zaentrum docs:
[Extending zaentrum](https://github.com/zaentrum/zaentrum/wiki/extending).

## License

[MPL-2.0](./LICENSE), like the rest of the platform.
