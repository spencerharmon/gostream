# gostream on Kubernetes — reference deployment pieces

Reference building blocks the `k8s-reference-manifests` capstone composes into the
Deployment/Service that flux's `phantom-library-bluegreen-deploy` consumes. Nothing
here is applied directly. Pieces:

- **Split config** — `configmap.yaml` + `secret.sops.example.yaml`, merged to one
  `/etc/gostream/config.json` at start (below).
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
