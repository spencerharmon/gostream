# zuul-ci — move gostream build/test onto self-hosted Zuul

Moves gostream's build/test (and container-image build) onto the swarm's
self-hosted **Zuul** — the CI deployed into the k3s cluster by the `flux`
submodule's `zuul` task — instead of GitHub Actions. Mirrors the canonical shape
of beehive's `release-verify` Zuul wiring.

## What ships

- **`.zuul.yaml`** — job + project definitions:
  - `gostream-build-test` — `gofmt -l` (must report zero files), `go vet`,
    `go build ./...`, the `gostream` binary build, and `go test -count=1 ./...`.
  - `gostream-image-build` — builds the container image from `docker/Dockerfile`
    for the node's native arch (`docker buildx --load`), proving the image still
    builds on every change.
  - `project:` attaches both jobs to `check` **and** `gate`.
- **`playbooks/gostream-build-test.yaml`**, **`playbooks/gostream-image-build.yaml`**
  — thin Ansible playbooks that resolve the checkout via the standard Zuul
  `{{ ansible_user_dir }}/{{ zuul.project.src_dir }}` pattern and shell into the
  build/test/image commands. Each opens with an honesty guard (below).

## gostream builds with cgo — the CGO_ENABLED=0 static convention does NOT apply

The shared Zuul-CI convention (see the hive's `skills/zuul-ci.md`, and beehive's
`release-verify`) is to build Go components `CGO_ENABLED=0` and assert the binary
is statically linked. **gostream cannot follow that**: it links FUSE through cgo
(`github.com/hanwen/go-fuse`), so it MUST build `CGO_ENABLED=1` and produces a
dynamically-linked binary. This matches `docker/Dockerfile` (which sets
`CGO_ENABLED=1` and installs `libfuse3-dev`, `build-essential`, `pkg-config`) and
the README "Build from Source" steps. Every Go step in `gostream-build-test`
therefore runs `CGO_ENABLED=1`; the build node must provide the Go toolchain
(>= `go.mod`'s `go 1.24.0`) plus `libfuse3-dev`, `build-essential` and
`pkg-config`. There is no static-link assertion because the artifact is, by
design, not static.

## Honest Nodepool gating — nothing is faked green

Both jobs run REAL build steps and REQUIRE an executor node:
`gostream-build-test` needs a Go/libfuse3 node; `gostream-image-build` needs a
Docker+buildx node. **flux has no Nodepool build-node provider deployed yet**, so
neither job can actually execute today.

Rather than stub the jobs green, honesty is enforced two ways:

1. **`nodeset:` is omitted** on both jobs (no guessed Nodepool label — the
   concrete label ships with flux's Nodepool work), so a job inherits whatever
   nodeset the tenant base job eventually defines.
2. **A localhost guard play** heads each playbook and `fail:`s the job when
   `groups['all']` is empty (no build node assigned). Without it, a `hosts: all`
   play with an empty inventory would match zero hosts, run nothing, and report a
   false **green**. The guard turns "no Nodepool node" into an honest **red**.
   Remove the guard once flux's Nodepool provider is live.

### Cross-dep on flux's Nodepool work (recorded for the next reconcile)

Live execution of this pipeline depends on flux standing up a Nodepool build-node
provider. That flux task **does not exist yet**, so no concrete `flux:<taskid>`
is invented here. This is recorded so the next reconcile can attach the qualified
cross-submodule dep `deps=flux:<nodepool-taskid>` to this task in `PLAN.md` once
flux's Nodepool task id is known. That qualified dep is authorized by a
`gostream` <-> `flux` link in the hive's `SUBMODULE-LINKS.yaml`; adding that
bidirectional link (a hive-layer edit, outside this code worktree) is a
prerequisite for wiring the dep — reconcile/operator should ensure it exists (the
reference precedent is beehive's `release-verify`, dep `flux:zuul`, authorized by
the `beehive` <-> `flux` link).

## GitHub Actions -> Zuul migration

`.github/workflows/docker-publish.yml` (tag-triggered; builds multi-arch images
and **publishes** to Docker Hub + GHCR, updates the Docker Hub description) is
**superseded** by the Zuul `gostream-image-build` check/gate job for the *build*
half — the job proves `docker/Dockerfile` still builds on every change.

The **publish** half (multi-arch cross-build, `docker push` to Docker Hub/GHCR,
Docker Hub description update) is a live-runtime concern needing registry secrets
and a dedicated tag-triggered pipeline, and is intentionally NOT reproduced in
the check/gate jobs — exactly as beehive kept cosign signing/publishing out of
its static release job. It belongs in a follow-up release pipeline once flux's
Zuul tenant grows a tag-triggered pipeline and the secret store bridges the
registry credentials.

`docker-publish.yml` is therefore **left in place for now** — deleting it before
the Zuul image-build+publish can actually run (no Nodepool yet) would leave
gostream with no working image-publish path at all. Cutover (removing
`docker-publish.yml`) is the documented follow-up once the Zuul pipeline is proven
live. The other `.github/workflows/*` files are Gemini bot automation, unrelated
to build/test CI, and are untouched.

## Verification (local, static — no live Zuul run)

- **YAML**: `.zuul.yaml` and both playbooks parse via `python3 yaml.safe_load`;
  structural checks confirm every `project:` check/gate job maps to a defined
  `job:` and every job's `run:` playbook exists on disk.
- **gofmt**: the `gostream-build-test` job enforces `gofmt -l .` == empty as a
  hard gate (mirroring beehive's `release-verify`). The tree carried pre-existing
  gofmt drift in 13 files; this task normalized them with `gofmt -w` (pure
  formatting, no semantic change) so the gate — and the hive's own handoff gofmt
  check — passes cleanly.
- **go vet**: the job runs `go vet ./...`. The tree had one pre-existing vet
  failure — `internal/ai/tuner.go` used an unescaped `%` in a `fmt.Sprintf`
  format string (`(target <60%)`), which vet reads as an unknown verb `)`. Fixed
  to `%%` (correct literal-percent escaping); `go vet ./...` is now clean.
- **Build commands**: identical to `docker/Dockerfile` / README "Build from
  Source", which already produce released images — so they are proven-correct.
  A local `CGO_ENABLED=1 go build` compiles all gostream + cgo sources and
  reaches the final link stage; in this sandbox the native link fails only on a
  pre-existing broken-toolchain issue (`ld: cannot find -latomic_asneeded`, the
  identical breakage beehive's `release-verify` recorded, plus GCC 16 being far
  newer than the Dockerfile's pinned `golang:1.24-bookworm` gcc-12, which trips a
  third-party cgo dep's legacy C). The pinned bookworm toolchain in
  `docker/Dockerfile` builds and links cleanly, as the published images show.
- **Live pipeline run, image push, and signing/publishing** are correctly OUT of
  scope here — they run on the deployed Zuul / by the operator once flux's
  Nodepool provides an executor node.
