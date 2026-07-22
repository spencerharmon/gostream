# gostream-image-build — build + publish the gostream image in-cluster on Zuul (ROI Priority 3)

Tier: P3. Code task in the gostream repo. deps: none (Nodepool build-node provider + registry now
DONE per ROI). Follows the flux runbook `docs/runbooks/build-image-in-gitea.md`.

## Why this task exists (ROI diff, 2026-07-20)

- The Nodepool build-node provider now EXISTS (flux, 2026-07-20) — no longer gated.
- `docker.io/mrrobotogit/gostream:testing` was **never actually published** — not a deploy source,
  never a fallback.
- The image MUST be built on Zuul's buildah node and published to the Gitea OCI registry as
  `git.spencerharmon.com/zuul/gostream:<tag>` (registry-served digest, never `:latest`). This
  pipeline IS the image supply — never escalate for a missing image, never depend on an external
  registry.
- flux's blue/green deploy (`phantom-library-bluegreen-repin-gitea-images`) consumes exactly this
  ref via the cross-dep `gostream:gostream-image-build`.

## Scope / what landed

- `.zuul.yaml`: split the old single `gostream-image-build` (a local, no-push build-sanity job)
  into `gostream-image-build-check` (keeps that sanity role) and a NEW `gostream-image-build` job
  that `parent: build-and-publish-image` (flux's reusable base job) with
  `vars: {image_name: gostream, image_context: ., containerfile: docker/Dockerfile}` — the exact
  shape the runbook's recipe step 3 specifies. All three jobs (`gostream-build-test`,
  `gostream-image-build-check`, `gostream-image-build`) attach to `post` — the only pipeline the
  tenant defines (see below); no `pipeline:` object is declared (untrusted-project constraint).
- `playbooks/gostream-image-build.yaml`: re-commented for the renamed
  `gostream-image-build-check` job (unchanged behavior — local `docker buildx --load`, no push).
- `ARTIFACTS.md` / `INFRASTRUCTURE.md` (beehive layer): recorded the new canonical published ref
  and marked the old `docker.io/mrrobotogit/gostream:testing` ref never-published/superseded.

## Blocking finding (cross-submodule, NOT fixed here — flux's to close)

Checked flux's tracked `infrastructure/zuul/tenant-config.yaml` directly (cloned
`https://git.spencerharmon.com/spencer/flux.git`, confirmed `origin/main` == the submodule pointer
`b0690dd3a4e98d169f7d2e78e6df465cbe8e625c`, i.e. this is the current tip, not a stale clone):

- The `zuul-config` config-project's seeded `jobs.yaml` defines only `base`, the `buildah-pod`
  nodeset, and `build-node-smoke` — **no `build-and-publish-image` job and no
  `gitea_registry_push` secret exist yet**, despite the runbook (and this task's own ROI text)
  describing them as already shipped.
- `main.yaml`'s tenant definition lists `untrusted-projects: [spencerharmon/flux,
  spencerharmon/helm-charts, spencerharmon/beehive]` — **`spencerharmon/gostream` is not
  registered**, so even a syntactically perfect `.zuul.yaml` in this repo cannot be loaded by the
  tenant yet.

Both are flux-owned infra (`infrastructure/zuul/tenant-config.yaml`), out of this task's file
scope (gostream repo + beehive-layer docs only) and out of this worktree's write authority. This
is a genuine missing-prerequisite gap, not an async-convergence wait (`skills/deferred-
verification.md` does not apply — there is no pending reconcile to poll; the job/secret/
registration simply do not exist in flux's tracked config yet). Per the runbook and this task's own
scope, closing it is flux's — a `build-and-publish-image` base job + `gitea_registry_push` secret +
`spencerharmon/gostream` untrusted-project registration, landed the same way `zuul-config`'s
existing `jobs.yaml`/`main.yaml` entries were.

## Verification performed

- `.zuul.yaml` parses as valid YAML (`python3 -c "import yaml; yaml.safe_load(open('.zuul.yaml'))"`
  — passes) and its top-level structure is exactly 3 `job:` objects
  (`gostream-build-test`, `gostream-image-build-check`, `gostream-image-build`) + 1 `project:`
  object attaching all three to `post` only, with **no `pipeline:` object** (confirmed by walking
  the parsed YAML's top-level keys) — satisfying the untrusted-project constraint.
- `playbooks/gostream-image-build.yaml` still parses as valid YAML after the comment rewrite.
- **NOT performed (documented, not silently skipped):** a live Zuul tenant-config load / `zuul
  --check-config` run, and the pull-by-digest confirmation of a published
  `git.spencerharmon.com/zuul/gostream:<tag>` image. Both require the flux-side prerequisites above
  to exist first — attempting either now would either hit "unknown parent job build-and-
  publish-image" / "project spencerharmon/gostream not found" (tenant load) or have no image to
  pull at all. No Zuul run has fired for this branch because the project isn't registered yet, so
  there is nothing to poll — this is not a bounded async wait to defer, it is an absent
  prerequisite.

## Status

Left `NEEDS-REVIEW`, not `DONE` — the gostream-side job/config work described above is complete and
believed correct per the runbook's recipe, but the Accept criterion's live-effect confirmation
(published image pullable by digest) cannot be performed until flux lands the
`build-and-publish-image` base job + `gitea_registry_push` secret + `spencerharmon/gostream`
untrusted-project registration. A reviewer should judge whether this qualifies as an honest partial
delivery (land the config now, re-verify once flux's side exists) or whether the task should instead
record a cross-submodule dependency note for the next gostream ROI reconcile to attach (mirroring
`docs/tasks/zuul-ci.md`'s prior Nodepool-cross-dep pattern), analogous to jellyfin's
`zuul-image-build-publish.md`, which recorded the identical gap.

## Live-effect verification (bee-gostream-image-build-verify, 2026-07-22)

flux's `zuul-build-publish-image-base-job` and `zuul-github-readonly-image-source` are now DONE and
live: the tenant's `/api/tenant/beehive/projects` lists `spencerharmon/gostream` over the `github`
connection, `/api/tenant/beehive/jobs` lists `gostream-image-build`, and there are zero tenant
config-errors. This non-config-file commit (docs only) is a deliberate `post`-pipeline trigger so a
plain content push (not a `.zuul.yaml` change) exercises the pipeline without hitting the git
driver's tenant-reconfigure path.

Retry after full zuul-scheduler restart (2026-07-22, second attempt) to confirm the `post` pipeline's
github trigger filter is active post-reload.

Retry 2026-07-22 (session gostream-1784750402-44655): docs-only push AFTER the current
zuul-scheduler baseline, to enqueue a real `post` gostream-image-build and confirm the
published image is pullable by digest from the Gitea OCI registry. Root cause of the earlier
no-build passes is now pinned: the Zuul git driver never sets `event.branch`, so a
config-updating (`.zuul.yaml`) ref-updated over the `github` connection raises
`TypeError: expected string or bytes-like object, got 'NoneType'` in
`scheduler._forward_trigger_event -> tpc.includesBranch(None)` and the event is dropped
(no build). A NON-config push avoids the tenant-reconfigure path and forwards normally.
