# gostream — Kubernetes reference manifests

Reference (single-color) Kubernetes manifests for deploying gostream into the
flux-managed **k3s** cluster on `spray`, co-located with patched Jellyfin under a
**blue/green** app tree. These are *references* that flux's
`phantom-library-bluegreen-deploy` consumes and adapts — the live blue/green color
overlays and the Jellyfin sidecar live in the flux repo, not here.

| File | Purpose | Owner task |
|------|---------|-----------|
| `state-pvc.yaml`  | Persistent state — SQLite inode map + caches (RWO, **never pruned**) | `state-pvc` |
| `warmup-pvc.yaml` | OPTIONAL SSD warm cache (RWO, wipe-safe) | `state-pvc` |

> Deployment/Service manifests and the Pod that *binds* these claims are added by
> `k8s-reference-manifests` (the integration capstone). This section delivers only
> the PersistentVolumeClaim(s) + the state-persistence contract they must honour.

## State persistence (`state-pvc.yaml`)

### Mount contract
- The `gostream-state` claim is mounted at **`/usr/local/state`** — the value of
  `GOSTREAM_ROOT_PATH`. The reference Pod (owned by `k8s-reference-manifests`)
  performs the actual bind; this task only delivers the claim + contract.
- Under that root: **`gostream.db` (SQLite) — the permanent inode map** giving every
  virtual `.mkv` a stable inode/ID across restarts *and* redeploys, plus sync state,
  negative caches, scheduler state, and `logs/`.

### Access mode — `ReadWriteOnce` (single node `spray`)
Deliberately `ReadWriteOnce`, **not** `ReadWriteOncePod`. RWO binds the volume to a
single *node*, which lets multiple pods on that node mount it read-write — exactly
what the shared blue/green model below needs on the single-node cluster.
`ReadWriteOncePod` would forbid the second color from mounting and break sharing.

### Blue/green decision — **SHARED** (one state PVC for both colors) — recommended
- **Decision:** blue and green **share the single `gostream-state` claim**; both
  color pods reference the same PVC name.
- **Why shared:** the inode map stays byte-identical across a color flip, so a
  blue→green cutover preserves every virtual-`.mkv` inode/ID and **Jellyfin item IDs
  never churn**. It is also simpler — one claim, one dataset, no copy/migration step
  on cutover.
- **Tradeoff (recorded):** a shared RWO claim means both colors can write the same
  `gostream.db`, and two simultaneous SQLite writers can hit `SQLITE_BUSY`. In
  practice blue/green keeps one color *live* at a time (the other staged), and
  SQLite's file locking bounds any brief cutover overlap to a single writer; flux's
  cutover sequences the flip so the retiring color quiesces before the new one takes
  over. Requires both colors to be schedulable on the same node — true here (single
  node `spray`).
- **Alternative — per-color PVCs — REJECTED:** full write isolation, but on cutover
  the new color starts with empty state and **re-derives the entire inode map →
  Jellyfin item IDs churn → the library breaks.** That is exactly the sacred-inode
  failure the ROI forbids. Only revisit if a future multi-node topology makes a
  shared RWO claim impossible, and even then a state **copy/replication** step to
  carry `gostream.db` forward becomes mandatory — never a fresh re-derive.

### Inode map is sacred — never wiped or rotated
- No manifest here (or downstream) may wipe, rotate, or re-init `gostream.db` as a
  deploy step. There is intentionally **no** init job/step that touches the db.
- The claim carries the flux label `kustomize.toolkit.fluxcd.io/prune: disabled` so
  **flux never garbage-collects it** — removing or redeploying the Kustomization
  leaves the state volume (and its inode map) intact.
- k3s `local-path` provisions with a **`Delete`** reclaim policy, so for defence in
  depth back the state volume with a `Retain` PV (or take backups): then even a
  manual `kubectl delete pvc` cannot destroy the inode map. (The warm cache below
  needs no such protection — it is reconstructible.)

### Sizing / storage class
- `storageClassName: local-path` — the built-in k3s provisioner on `spray`. Override
  in the flux overlay for a different class.
- `5Gi` request is a reference default (the SQLite db + caches are small but grow
  with library size); tune per library.

## Optional warm cache (`warmup-pvc.yaml`)
- `gostream-warmup` is an **optional** SSD-backed cache mounted at
  `/usr/local/state/warmup`. Deploy only if a fast warm cache is wanted.
- It holds **no** permanent state — purely reconstructible cache data — so it is
  **wipe-safe** and deliberately does **not** carry the prune-disabled guard. Point
  its `storageClassName` at an SSD class if one exists on `spray`.

## Validate
```sh
kubectl apply --dry-run=client -f state-pvc.yaml
kubectl apply --dry-run=client -f warmup-pvc.yaml
```
