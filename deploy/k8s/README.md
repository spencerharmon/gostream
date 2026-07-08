# gostream on Kubernetes — reference deployment pieces

Reference building blocks that the `k8s-reference-manifests` capstone composes into the
single-color Deployment/Service that flux's `phantom-library-bluegreen-deploy` consumes
and adapts. Nothing here is applied directly from this repo — flux owns the live
blue/green overlay. Pieces:

- **Reference Deployment + Service (the capstone)** — `deployment.yaml`,
  `service.yaml`, `kustomization.yaml`: the single-color gostream spec that composes
  the four pieces below (`kubectl kustomize deploy/k8s` builds it). See "Reference
  Deployment/Service (the capstone)" and "Networking: ClusterIP Service, not
  hostNetwork".
- **Split config** — `configmap.yaml` + `secret.sops.example.yaml`, merged to one
  `/etc/gostream/config.json` at start (below).
- **State persistence** — the `gostream-state` PVC (shipped by the `state-pvc` task's
  `state-pvc.yaml`), bound at `/usr/local/state`; the sacred `gostream.db` inode map is
  never wiped.
- **Health probes** — `/healthz` (liveness) + `/readyz` (readiness) on `:9080` (the
  `healthz-probe` task), wired into the Deployment.
- **FUSE mount propagation** — `pod-fuse-fragment.yaml`, how gostream's in-container
  FUSE mount surfaces to a co-located jellyfin (see "FUSE mount propagation to
  co-located jellyfin").

## Split config (ConfigMap + Secret)

gostream loads exactly **one** file, `/etc/gostream/config.json` (env
`MKV_PROXY_CONFIG_PATH`). For Kubernetes that single file is split into:

- **`configmap.yaml`** — `ConfigMap/gostream-config`, non-secret **tuning** only.
- **`secret.sops.example.yaml`** — `Secret/gostream-config-secret`, the named
  **tokens** only. Committed **schema-only** (empty values, zero plaintext); the
  real, populated Secret is **SOPS-encrypted and owned by flux** in its own tree.

At container start the entrypoint **deep-merges** the two fragments back into one
`/etc/gostream/config.json`. Pod wiring is done by the reference-manifests
capstone; this directory defines the pieces it wires.

## Key split (derived from `repo/config.json.example`)

The split is taken **verbatim** from `config.json.example` — no keys are invented.
Exactly three credentials exist in that file; everything else is tuning.

| Destination | Keys |
|-------------|------|
| **Secret** (`config.secret.json`) | `plex.token`, `tmdb_api_key`, `library_api_token` |
| **ConfigMap** (`config.tuning.json`) | every other key: concurrency/cache/buffer/timeout tuning, `log_level`, ports, `gostorm_url`, `blocklist_url`, paths, `natpmp.*`, **`plex.url`**, **`plex.library_id`**, library timeouts, … |

`plex` is a **split object**: its non-secret members (`url`, `library_id`) stay in
the ConfigMap, and only `plex.token` moves to the Secret. This is exactly why the
merge must be a **recursive/deep** merge (below), not a shallow key overwrite.

> Not present in `config.json.example`, so intentionally **not** added here:
> `prowlarr.api_key` (Prowlarr is optional/disabled in the example). If Prowlarr
> is enabled later, put `prowlarr.api_key` in the Secret as
> `"prowlarr": { "api_key": "…" }` — the recursive merge handles the nested
> object automatically, just like `plex`.

## Merge mechanism (chosen: entrypoint jq deep-merge onto an emptyDir)

`docker/docker-entrypoint.sh` runs, when **both** of these env vars are set:

```
MKV_PROXY_CONFIG_TUNING_PATH  -> mounted config.tuning.json (from the ConfigMap)
MKV_PROXY_CONFIG_SECRET_PATH  -> mounted config.secret.json (from the Secret)
```

```sh
jq -s '.[0] * .[1]' "$TUNING" "$SECRET" > "$MKV_PROXY_CONFIG_PATH"
```

`jq`'s `*` operator does a **recursive** object merge with the **right operand
(secret) winning**, so `{plex:{url,library_id}} * {plex:{token}}` yields
`{plex:{url,library_id,token}}` — one valid `config.json`. The write is atomic
(temp file + `mv`) and **no secret values are ever logged**. When the vars are
unset the merge is skipped entirely, so Docker/systemd single-file installs are
unchanged (backward compatible).

**Why this and not the alternatives:**

- **Projected volume — rejected.** A projected volume surfaces the ConfigMap and
  Secret as *separate files*; it cannot deep-merge the split `plex` object into a
  single file, and gostream reads exactly one `config.json`.
- **Standalone-image initContainer — rejected.** A deep JSON merge needs `jq`, and
  the official `ghcr.io/jqlang/jq` image is shell-less (jq is the entrypoint), so
  it can't redirect merged stdout to a file; that forces a third-party `sh`+`jq`
  image. Since `jq` must live *somewhere* anyway, adding it to the gostream image
  (which already ships `/bin/sh`) keeps everything first-party, air-gap-friendly,
  and testable — at the cost of one `jq` package (`docker/Dockerfile`) and a
  guarded block in the entrypoint. (Coordinated with the `k8s-image` task.)

The merged output has the **same key set and shape as `config.json.example`**, so
it loads into gostream's config struct unchanged.

## SOPS / secret handling

- `secret.sops.example.yaml` in this repo is the **schema** — empty token strings,
  **zero plaintext token bytes**. Safe to commit.
- The **real** Secret is produced and encrypted with **SOPS in flux's tree**, never
  committed here in plaintext. Example encryption:

  ```sh
  sops --encrypt --encrypted-regex '^(data|stringData)$' \
       gostream-config-secret.plaintext.yaml > gostream-config-secret.sops.yaml
  ```

  flux decrypts it in-cluster; the running Secret is named
  `gostream-config-secret` in namespace `gostream`.

## Pod wiring (reference for the `k8s-reference-manifests` capstone)

Not applied from this directory — shown so the capstone wires it consistently.
Mount the two fragments read-only at distinct source paths, give the merged output
a writable `emptyDir`, and point the env vars at them:

```yaml
containers:
  - name: gostream
    env:
      - { name: MKV_PROXY_CONFIG_PATH,        value: /etc/gostream/config.json }
      - { name: MKV_PROXY_CONFIG_TUNING_PATH, value: /config-src/tuning/config.tuning.json }
      - { name: MKV_PROXY_CONFIG_SECRET_PATH, value: /config-src/secret/config.secret.json }
    volumeMounts:
      - { name: config-merged, mountPath: /etc/gostream }
      - { name: config-tuning, mountPath: /config-src/tuning, readOnly: true }
      - { name: config-secret, mountPath: /config-src/secret, readOnly: true }
volumes:
  - name: config-merged
    emptyDir: {}
  - name: config-tuning
    configMap: { name: gostream-config }
  - name: config-secret
    secret: { secretName: gostream-config-secret }
```

## Validate

```sh
# Manifest schema (client-side, no cluster needed):
kubectl create --dry-run=client -f deploy/k8s/configmap.yaml -o name
kubectl create --dry-run=client -f deploy/k8s/secret.sops.example.yaml -o name

# Merge logic, locally with jq (fill test values into a secret fragment first):
jq -s '.[0] * .[1]' config.tuning.json config.secret.json | jq .plex
```

## FUSE mount propagation to co-located jellyfin

gostream FUSE-mounts a virtual `.mkv` tree at its config `fuse_mount_path`
(`/mnt/gostream-mkv-virtual`, backed by `physical_source_path`
`/mnt/gostream-mkv-real`) and Jellyfin must read that tree. On the host today this is
**mount propagation**, not a network share: gostream FUSE-mounts, and a host
`rshared` bind at **`/var/gostream/gostream-mkv-virtual`** makes that mount visible to
Samba/Jellyfin. In-cluster we reproduce exactly that with Kubernetes
`mountPropagation`. The reference is `pod-fuse-fragment.yaml`; this section is the
*why*.

## The propagation pairing (one Pod, two containers)

gostream (producer) and jellyfin (consumer) run as **two containers in one Pod** and
share one volume, `virtual-mkv`:

| Container | mountPropagation | Linux equiv | Privilege | Access |
|-----------|------------------|-------------|-----------|--------|
| **gostream** | `Bidirectional` | `rshared` | **privileged** (+ `/dev/fuse`, `CAP_SYS_ADMIN`) | mounts FUSE at `/mnt/gostream-mkv-virtual` |
| **jellyfin** | `HostToContainer` | `rslave` | unprivileged | read-only at `/media/gostream` |

Mechanism:

1. Both containers mount the **same** `virtual-mkv` source, so they share one host
   mount **peer group**.
2. gostream's mount is `Bidirectional` (`rshared`). It FUSE-mounts **on top of** that
   mount, so the new FUSE mount **propagates UP** to the host peer group.
3. jellyfin's mount of the same source is `HostToContainer` (`rslave`), so the FUSE
   mount **propagates DOWN** into jellyfin. It appears, and disappears again when
   gostream unmounts — the same live behavior as the host `rshared` bind.
4. Propagation keys off the **shared source**, not a matching in-container path — the
   two `mountPath`s differ (`/mnt/gostream-mkv-virtual` vs `/media/gostream`) on
   purpose to make that explicit. Point Jellyfin's library at its path.
5. gostream creates the FUSE mount with **`allow_other`** (`main.go`: `AllowOther:
   true`). Running privileged/root satisfies libfuse's `allow_other` gate without an
   `/etc/fuse.conf` edit, so jellyfin's UID can traverse the tree.

> **Ordering / restart:** because jellyfin is `rslave`, a FUSE mount gostream creates
> *after* jellyfin starts still propagates in, and a gostream restart re-propagates a
> fresh mount — no jellyfin restart needed. An unclean gostream exit can leave a stale
> mount in the peer group until `fusermount3 -u` cleans it (`k8s-image` makes the
> normal TERM path unmount cleanly).

## Shared-volume choice: hostPath (chosen), grounded in propagation semantics

Mount propagation is a **host-mount-namespace** mechanism: the event travels
gostream(`rshared`) → host peer group → jellyfin(`rslave`). For that peer group to
exist, the shared source must be a real host mount that CRI makes `rshared` under
`Bidirectional`, and the node's parent mount must itself be shared.

- **`hostPath` at `/var/gostream/gostream-mkv-virtual` — CHOSEN.** It is the literal
  analog of today's host `rshared` bind (same path), it is the pattern the Kubernetes
  docs document for `Bidirectional`, and it survives container restarts (the host dir
  persists; gostream re-mounts into it). Node prerequisite: `/` (or the parent of that
  path) must be `rshared` — this is systemd's default (`mount --make-rshared /` at
  boot) and is the exact k8s analog of the host bind's `--make-rshared`.
- **`emptyDir` — considered, NOT default (tradeoff).** Pod-scoped and self-contained
  (no host-path dependency), and propagation *can* work through it because both
  containers bind-mount the one kubelet-managed dir that CRI makes `rshared` under
  `Bidirectional`. Rejected as the default because: (a) it is less proven / more
  fragile than the documented `hostPath` pattern; (b) the shared dir and any stale
  FUSE mount are torn down with the Pod, which can complicate teardown; (c) it diverges
  from the proven host analog. Revisit only if a host-path dependency is unacceptable.

## Privileged-FUSE security tradeoff (recorded)

Kubelet permits `Bidirectional` mountPropagation **only for privileged containers**,
so gostream must set `securityContext.privileged: true`. That is stronger than the
FUSE mount alone needs (`/dev/fuse` + `CAP_SYS_ADMIN`): **you cannot keep
`Bidirectional` with only `SYS_ADMIN` + `/dev/fuse`** — kubelet's admission check keys
on the `privileged` flag itself. Privileged = full host device access + all
capabilities + the power to damage the host OS via propagated mounts; it is the single
largest attack-surface item in this deployment.

Bounding it (kept in the reference):

- **Only gostream is privileged.** jellyfin stays unprivileged, `HostToContainer`,
  `readOnly` — the consumer needs no privilege, it only reads.
- **Scope the hostPath tightly** to `/var/gostream/gostream-mkv-virtual` (not `/`, not
  a broad parent), and run on the single, single-tenant node (`spray`).
- **Non-default way to drop privileged:** expose `/dev/fuse` via a device plugin
  (e.g. `smarter-device-manager`) and grant only `CAP_SYS_ADMIN` — but that **loses
  `Bidirectional`**, so it only fits the SMB/CSI decoupling below, not this propagation
  design. Untested here; do not adopt silently.

## SMB / CSI — explicit UNTESTED non-default

The operator named an **SMB/CSI** alternative: instead of sharing a mount namespace,
gostream re-exports the FUSE tree over **Samba** (as the host does for Samba today) and
jellyfin mounts it as a network share via a CSI driver (e.g. `csi-driver-smb`). This is
recorded as an explicit **non-default** and is **NOT silently chosen**:

- **Pro:** decouples the containers — no shared privileged mount namespace, no
  `Bidirectional`, jellyfin just needs the network + credentials; the two need not even
  co-locate.
- **Con:** adds an SMB server + a CSI driver + credentials (a SOPS Secret) + a network
  hop and its caching/latency, all of which is **UNTESTED in this cluster**. It trades
  the propagation design's privileged gostream for a larger moving-part count.

Default remains mount propagation (above) because it is the direct, proven analog of
the working host setup. SMB/CSI is a documented fallback to evaluate only if the
privileged-gostream tradeoff is rejected — it must be validated before use.

## Validate the FUSE fragment

```sh
# Fragment schema (client-side, offline — no cluster needed):
kubectl create --dry-run=client -f deploy/k8s/pod-fuse-fragment.yaml -o name
```

**Live in-cluster propagation verification is deferred to flux's
`phantom-library-bluegreen-deploy`** (it owns the live blue/green bring-up). Do NOT
claim a live propagation pass from this repo — this task ships the design + the
reference fragment only. In-cluster, the check is: exec into jellyfin and confirm the
gostream FUSE tree is visible read-only under its mountPath while gostream is running.

# Reference Deployment/Service (the capstone)

`deployment.yaml` + `service.yaml` + `kustomization.yaml` are the **integration
capstone** (`k8s-reference-manifests`): the single-color gostream reference that flux's
`phantom-library-bluegreen-deploy` consumes and adapts. `kubectl kustomize deploy/k8s`
builds the composed base (Deployment + Service + tuning ConfigMap). It is a
**reference**, not a live apply target — flux layers the blue/green color patches, the
jellyfin sidecar, and the real SOPS Secret on top.

## What it composes (each dep → what it contributes)

| Dep task | Contributes | Where in the Deployment |
|----------|-------------|-------------------------|
| **fuse-mount-propagation** | shared `virtual-mkv` hostPath + gostream's `Bidirectional`/`privileged` FUSE mount; the seam for flux's jellyfin `HostToContainer` RO sidecar | `virtual-mkv` volume + mount at `/mnt/gostream-mkv-virtual` |
| **state-pvc** | `gostream-state` PVC bound at `/usr/local/state` (`GOSTREAM_ROOT_PATH`); `gostream.db` inode map never wiped | `state` volume (`claimName: gostream-state`) |
| **config-secret** | ConfigMap `gostream-config` + Secret `gostream-config-secret`, deep-merged by the entrypoint into one `/etc/gostream/config.json` | `config-tuning`/`config-secret` (RO sources) + `config-merged` emptyDir |
| **healthz-probe** | `/healthz` (liveness) + `/readyz` (readiness) on `:9080`, no external-upstream flapping | `livenessProbe`/`readinessProbe` |
| **k8s-image** (sibling) | the tuned `docker.io/mrrobotogit/gostream:testing` image + port/GOMEMLIMIT/fail-loud contract (`ARTIFACTS.md`) | container `image`, ports, `GOMEMLIMIT` |

### Env / state contract (do not drift from the entrypoint)

The entrypoint (`docker/docker-entrypoint.sh`) reads these; the Deployment sets them so
the PVC mount, FUSE paths, and merged config all agree:

- `MKV_PROXY_CONFIG_PATH=/etc/gostream/config.json` — the single merged file gostream loads.
- `MKV_PROXY_CONFIG_TUNING_PATH` / `MKV_PROXY_CONFIG_SECRET_PATH` — the two split sources
  the entrypoint `jq`-deep-merges (see "Merge mechanism").
- `GOSTREAM_ROOT_PATH=/usr/local/state` — equals the state PVC mountPath, so
  `GetStateDir()` (`$ROOT_PATH/STATE`) lands `gostream.db` on the persistent claim. The
  image's own default is `/usr/local`; the k8s contract is `/usr/local/state` and this
  reference sets it explicitly so the mount and env agree (see the `state-pvc` doc).
- `GOSTREAM_MOUNT_PATH=/mnt/gostream-mkv-virtual` / `GOSTREAM_SOURCE_PATH=/mnt/gostream-mkv-real`
  — the FUSE mount target (== `virtual-mkv`, Bidirectional) and the reconstructible
  library-stub source (== `real-mkv`, an emptyDir here).
- `GOMEMLIMIT=2200MiB` — carried from `gostream.service`, set BELOW the container memory
  limit (see below).

### What flux adds on top (kept OUT of this single-color reference on purpose)

- The **jellyfin sidecar** — a second container in the same Pod, mounting `virtual-mkv`
  `HostToContainer` RO at its own path to receive gostream's FUSE mount by propagation
  (illustrated end-to-end in `pod-fuse-fragment.yaml`).
- The **blue/green color overlay** — per-color name/label patches + a second replica set;
  both colors reference the SAME `gostream-state` claim so the inode map is byte-identical
  across a flip.
- The **real Secret** — `gostream-config-secret`, SOPS-encrypted in flux's tree
  (`secret.sops.example.yaml` here is schema-only).
- The `spray` **nodeSelector/affinity** — both colors must co-locate on the one node that
  owns the FUSE hostPath + RWO state PVC (left out here so the reference carries no
  site-specific node name).

## Networking: ClusterIP Service, not hostNetwork (decision recorded)

**Decision: pod networking + a ClusterIP Service; NOT `hostNetwork`.** The ROI asked to
"decide + document hostNetwork vs ClusterIP Service for peer/torrent connectivity."

**Why ClusterIP over hostNetwork:** blue/green (flux's job, which this reference must
feed) needs two colors running **at once on the single node** during a cutover.
`hostNetwork: true` puts the pod in the node's network namespace, so both colors would
fight for the same host ports (`:8080`/`:9080`/`:8090` **and** the torrent listen port)
— they cannot coexist, which breaks the whole blue/green model. A ClusterIP Service
gives each color its own Pod IP and lets flux flip the Service selector (blue↔green) to
cut traffic over atomically. It is also the standard, isolatable choice.

**Peer/torrent inbound connectivity** (the reason hostNetwork was historically used) is
handled WITHOUT hostNetwork:

- gostream's built-in **NAT-PMP** (`config natpmp.*`, `enabled: false` by default) maps
  the peer/torrent port on the VPN gateway. Any packet-filter / port-map work it performs
  MUST use the **nft / iptables-nft** backend **only** — never legacy iptables. The image
  ships Debian bookworm's `iptables`, which is the **nft-backed** `iptables` by default
  (`iptables-nft`), so gostream's `sudo iptables ...` calls already go through nftables;
  do not add or depend on `iptables-legacy` tooling. NAT-PMP needs `CAP_NET_ADMIN`, which
  `privileged: true` already grants.
- If a **fixed inbound peer port** is ever required without NAT-PMP, add a
  `hostPort`/NodePort for **just that torrent port** on a single color — do NOT switch the
  whole pod to `hostNetwork`. (A hostPort still lets the two colors differ on the HTTP
  ports; only the one torrent port is node-bound, and only one color exposes it.)

**`:8090` exposure note:** the GoStorm engine binds **all interfaces** (not loopback,
despite the `127.0.0.1` in `gostorm_url` — see `ARTIFACTS.md`). In the live overlay,
consider a `NetworkPolicy` restricting `:8090` to same-Pod / trusted traffic.

## Resource limits + GOMEMLIMIT

- `requests: cpu 500m / memory 1Gi`, `limits: cpu 2 / memory 3Gi`.
- `GOMEMLIMIT=2200MiB` is a **soft** heap ceiling set intentionally **below** the 3Gi
  hard limit: Go GCs aggressively as the heap approaches it, so memory pressure shows up
  as GC (recoverable) rather than a cgroup OOM-kill, with ~800Mi of headroom left for the
  cgo/FUSE + torrent **off-heap** buffers (`read_ahead_budget_mb`, metadata cache, libutp,
  etc.). A plain image `ENV` is a default; this Deployment `env:` overrides it per the
  `k8s-image` contract. flux may retune per color/host.

## Privileged + securityContext (reference tradeoff, recap)

The Deployment sets `securityContext.privileged: true` on gostream. This is **required,
not tunable**: kubelet permits `Bidirectional` mountPropagation only for privileged
containers, and privileged also supplies `/dev/fuse` + `CAP_SYS_ADMIN` (the FUSE mount)
and `CAP_NET_ADMIN` (NAT-PMP/nft). It is bounded exactly as the FUSE section describes
(only gostream privileged; jellyfin sidecar unprivileged + RO; tightly scoped hostPath;
single-tenant node). The non-default way to drop privileged — a `/dev/fuse` device plugin
+ `capabilities.add: [SYS_ADMIN, NET_ADMIN]` — **loses `Bidirectional`** and therefore
forces the SMB/CSI decoupling (also documented above), so it is not the reference default.

## Validate the capstone

```sh
# Compose the single-color base (client-side, offline — no cluster needed):
kubectl kustomize deploy/k8s

# Or validate the raw manifests individually:
kubectl create --dry-run=client -f deploy/k8s/deployment.yaml -o name   # -> deployment.apps/gostream
kubectl create --dry-run=client -f deploy/k8s/service.yaml    -o name   # -> service/gostream
```

**Live in-cluster bring-up is flux's `phantom-library-bluegreen-deploy`** — do NOT claim
a live deploy from this repo. This task ships the reference spec only; the live check is
flux's (pod schedules privileged on `spray`, FUSE mounts, `/readyz` goes green, jellyfin
sees the propagated tree, the Service fronts the live color). Cross-referenced, not
claimed here.
