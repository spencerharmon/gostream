# Task: fuse-mount-propagation — surface gostream's FUSE mount to co-located jellyfin

Design + record HOW gostream's in-container FUSE mount reaches a co-located Jellyfin
the same way it reaches the host today: mount **propagation**, not a network share.
Deliverables: `deploy/k8s/pod-fuse-fragment.yaml` + the "FUSE mount propagation" section
of `deploy/k8s/README.md`. No live cluster change here — that is flux's job (below).

## Ground truth (from repo, restated so nobody re-derives)

- gostream FUSE-mounts at config `fuse_mount_path` = `/mnt/gostream-mkv-virtual`
  (source `physical_source_path` = `/mnt/gostream-mkv-real`); `config.json.example`.
- The mount is created with `AllowOther: true` (`main.go` `fs.Mount(... MountOptions{
  AllowOther: true ...}`) — non-mounter UIDs (jellyfin) may traverse it.
- FUSE needs `/dev/fuse` + `CAP_SYS_ADMIN`; the image fails loud without them
  (`k8s-image`, `docker/docker-entrypoint.sh` `require_fuse()`).
- Host today: systemd runs gostream and a host `rshared` bind at
  `/var/gostream/gostream-mkv-virtual` propagates the FUSE mount to Samba/Jellyfin.

## The propagation pairing (one Pod, two containers, one shared volume)

- **gostream** (producer): `mountPropagation: Bidirectional` (== `rshared`),
  `securityContext.privileged: true`, mounts `virtual-mkv` at `/mnt/gostream-mkv-virtual`
  and FUSE-mounts on top of it.
- **jellyfin** (consumer): `mountPropagation: HostToContainer` (== `rslave`),
  unprivileged, `readOnly: true`, mounts the SAME `virtual-mkv` at `/media/gostream`.
- Flow: gostream's FUSE mount propagates UP to the host peer group (rshared) and DOWN
  into jellyfin (rslave). It keys off the shared **source**, not a matching mountPath
  (the two paths differ on purpose). `allow_other` + privileged/root lets jellyfin read.
- rslave means a mount created/replaced *after* jellyfin starts still propagates in →
  a gostream restart needs no jellyfin restart.

## Shared-volume choice — hostPath (grounded), emptyDir rejected as default

Propagation is a host-mount-namespace mechanism, so the shared source must be a real
host mount CRI makes `rshared` under `Bidirectional`.

- **hostPath `/var/gostream/gostream-mkv-virtual` — CHOSEN.** Literal analog of the
  host `rshared` bind (same path), the k8s-documented `Bidirectional` pattern, and it
  survives container restarts. Node prereq: `/` `rshared` (systemd default `mount
  --make-rshared /`) — the k8s analog of the host bind's `--make-rshared`.
- **emptyDir — considered, NOT default.** Self-contained/pod-scoped and propagation
  *can* work through it, but it is less proven/more fragile than hostPath, is torn down
  (with any stale mount) at Pod death, and diverges from the proven host analog.

## Privileged-FUSE security tradeoff (recorded)

Kubelet allows `Bidirectional` **only for privileged containers** → gostream MUST be
`privileged: true`. That is stronger than FUSE's own `/dev/fuse` + `CAP_SYS_ADMIN`
need: you **cannot** keep `Bidirectional` with only `SYS_ADMIN` — admission keys on the
`privileged` flag. Privileged = full host device + all caps + can damage host: the
biggest attack-surface item here. Bounded by: only gostream privileged (jellyfin stays
unprivileged/RO); tightly scoped hostPath; single-tenant node `spray`. A device-plugin
`/dev/fuse` + `SYS_ADMIN`-only path exists but loses `Bidirectional`, so it fits only
the SMB/CSI fallback — untested, not silently adopted.

## SMB / CSI — explicit UNTESTED non-default

Operator-named alternative: gostream re-exports the FUSE tree over Samba, jellyfin
mounts it via a CSI SMB driver (`csi-driver-smb`). Decouples the containers (no shared
privileged namespace, no co-location) at the cost of an SMB server + CSI driver +
credentials + a network hop/caching — **UNTESTED in this cluster**. Recorded as a
documented fallback ONLY; NOT the default and NOT silently chosen. Default stays mount
propagation (the proven host analog).

## Files

- `deploy/k8s/pod-fuse-fragment.yaml` — reference two-container Pod fragment (the
  propagation pairing + shared hostPath). Reference only, not applied.
- `deploy/k8s/README.md` — "FUSE mount propagation to co-located jellyfin": pairing,
  grounded volume choice, privileged tradeoff, SMB/CSI non-default, validation.

## Validation

- `kubectl create --dry-run=client -f deploy/k8s/pod-fuse-fragment.yaml -o name` → ok
  (offline schema check); PyYAML parse confirms the two containers, the
  `Bidirectional` / `HostToContainer`+`readOnly` propagation pair, and the hostPath.
- **Live in-cluster propagation is deferred to flux's `phantom-library-bluegreen-deploy`**
  and cross-referenced — NOT claimed here. In-cluster check: exec into jellyfin, confirm
  the gostream FUSE tree is visible read-only while gostream runs.

## Handoff to `k8s-reference-manifests` (capstone)

Consume `pod-fuse-fragment.yaml`: gostream `Bidirectional` + privileged + `/dev/fuse`;
jellyfin `HostToContainer` RO; shared hostPath `virtual-mkv`. Add the config-secret
(`/etc/gostream`), state-pvc (`/usr/local/state`), probes (`:9080`), Service/ports and
limits around it. jellyfin here is a placeholder — flux supplies the real patched image
and owns the live blue/green overlay + verification.
