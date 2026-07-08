# gostream on Kubernetes

Reference design notes and manifest fragments for running gostream under Kubernetes. flux's
`phantom-library-bluegreen-deploy` app tree consumes and adapts what lives in this directory; this repo
does not run a live cluster and does not itself perform in-cluster verification — each section below
says explicitly what is deferred and to where.

This file grows with each P1/P2/P3 gostream k8s task (`fuse-mount-propagation`, `state-pvc`,
`config-secret`, `k8s-reference-manifests`); each owns its own section below.

## FUSE Mount Propagation (gostream ↔ jellyfin) — ROI Priority 1

### Goal

Design and record HOW gostream's in-container FUSE mounts surface to a co-located jellyfin exactly as
they surface to the host today — via mount **propagation**, not a network share — and emit the
reference fragment `k8s-reference-manifests` composes into the full Deployment/Pod.

### Today, outside k8s (ROI fact, not re-derived here)

The host directory `/var/gostream/gostream-mkv-virtual` (config `fuse_mount_path`, env
`GOSTREAM_MOUNT_PATH` → `/mnt/gostream-mkv-virtual` in-container) is bind-mounted **`rshared`**
(`mount --make-rshared`), so the FUSE filesystem gostream mounts inside its container **propagates
back out to the host**; a host-resident jellyfin reads the virtual `.mkv` files directly. **No samba**
in this path. `physical_source_path` (`/mnt/gostream-mkv-real`) is the separate directory where gostream
writes stub files that the FUSE layer overlays; it is not part of this shared/propagated volume.

Two supporting facts already true in this codebase, so the k8s design below adds no new gostream code:

- The FUSE mount is created with `AllowOther: true` (`main.go`, `fs.Mount(...)` call) — a precondition
  for a *different* process (jellyfin, in a different container/UID) to read files through it at all,
  independent of whether the mount is visible there in the first place. gostream's container also runs
  as root by default (`docker/Dockerfile` has no `USER`), so `allow_other` is honored without needing an
  `/etc/fuse.conf` `user_allow_other` override.
- `docker/docker-entrypoint.sh` already treats `$GOSTREAM_MOUNT_PATH` as a pre-existing mountpoint
  (detects and cleans up a stale `fuse.*` layer left over it at startup) and already unmounts cleanly
  with `fusermount3 -uz` on `INT`/`TERM`/`EXIT`. That is exactly the discipline Kubernetes requires of
  `Bidirectional` mount propagation ("any volume mounts created by containers in Pods must be destroyed
  (unmounted) by the containers on termination" — see below) — already satisfied, no entrypoint change
  needed for this task.

### Target design: one Pod, two containers, one propagated volume

gostream and jellyfin are deployed as **two containers in one Pod** (see
`deploy/k8s/pod-fuse-fragment.yaml`):

- A single shared volume (`virtual-mkv`) is mounted into both containers at
  `/mnt/gostream-mkv-virtual` — the same path gostream already defaults to, so no env override is
  needed on the gostream side.
- **gostream** mounts it `mountPropagation: Bidirectional`. This is the side that *creates* the FUSE
  mount, so its propagation must flow outward: to the node, and to any other container/Pod sharing the
  volume — which is exactly how the sibling jellyfin container sees it.
- **jellyfin** mounts the SAME volume `mountPropagation: HostToContainer`, **read-only**. It only ever
  needs to *receive* the mount gostream created; it must never be able to create/alter mounts that leak
  back out.
- This pairing is the k8s-native equivalent of today's host `rshared` bind: gostream's side is the
  propagation source (`rshared`/`Bidirectional`), jellyfin's side is a plain propagation receiver
  (`rslave`/`HostToContainer`) — see [Kubernetes: Mount propagation][k8s-mount-propagation] for the
  `rshared`/`rslave`/`rprivate` equivalence.
- jellyfin's own container spec (image, its `/var/lib/jellyfin` + `/etc/jellyfin` persistence, the
  library path this volume is mounted under) is **not** this task's deliverable — see
  "Cross-references" below.

### Why propagation, not a network share

Propagation replicates today's `rshared` bind semantics with nothing new in the data path: the kernel
serves the same FUSE mount to both containers directly, with the same inode numbers gostream's
persisted inode map (`gostream.db`) already guarantees are stable. A network share (Samba/NFS/CSI)
would insert a client filesystem with its own, separate inode/identity handling in front of a
same-node, same-Pod hop that the kernel already provides for free — see "SMB/CSI" below for why that
extra layer is recorded as a non-default rather than silently adopted.

### Volume type: `hostPath`, not (plain) `emptyDir`

The Kubernetes project's own caution on mount propagation is the deciding fact here, not a preference:

> Mount propagation is a low-level feature that does not work consistently on all volume types. The
> Kubernetes project recommends only using mount propagation with `hostPath` or memory-backed
> `emptyDir` volumes. See [Kubernetes issue #95049][k8s-issue-95049] for more context.
> — [Kubernetes: Mount propagation][k8s-mount-propagation]

That excludes a plain (disk-backed) `emptyDir` outright — it is not a candidate regardless of
preference. Between the two remaining, sanctioned backings:

- **`hostPath` at `/var/gostream/gostream-mkv-virtual` (`type: DirectoryOrCreate`) is the default** in
  the reference fragment. It is the literal analog of today's host `rshared` bind — identical path, so
  it is trivially inspectable from the node (`ls /var/gostream/gostream-mkv-virtual`) exactly as today,
  and any on-node tooling that already expects that path keeps working unmodified. Because gostream and
  jellyfin are co-located in one Pod by construction, a Pod is inherently scheduled to a single node —
  `hostPath`'s single-node coupling, normally a portability concern, costs nothing extra here.
- **Memory-backed `emptyDir` (`medium: Memory`)** is recorded as a documented, equally-valid
  alternative (commented out in the fragment) for a fully Pod-scoped deployment: no stable node path
  to pre-provision, automatic cleanup on Pod deletion. It trades today's node-path parity for that
  scoping — pick it instead of `hostPath` if that trade is wanted. It is a mountpoint directory only
  (the FUSE layer serves file content from gostream's own RAM/SSD cache tiers, not from this volume's
  backing store), so the tmpfs memory cost is negligible.

On "provably surfaces to jellyfin": propagation correctness is a property of the `mountPropagation`
field plus using one of the two sanctioned backings, not of which sanctioned backing is chosen. Per the
`rshared`/`rslave` equivalence documented above, `Bidirectional`/`HostToContainer` give each container's
volume mount the same shared/slave propagation relationship whether the underlying directory is a real
host path or a kubelet-managed `emptyDir` directory — both are real directories on the node's
filesystem that the container runtime bind-mounts into each container with the propagation flag the
Pod spec requests. The choice between them is about node-path parity and volume lifecycle, not about
whether propagation happens — `hostPath` wins here on parity with today's design, not on correctness
`emptyDir` would lack.

### The privileged-FUSE security tradeoff

`Bidirectional` mount propagation is not a request Kubernetes will grant lightly:

> `Bidirectional` mount propagation can be dangerous. It can damage the host operating system, and
> therefore, it is allowed only in privileged containers.
> — [Kubernetes: Mount propagation][k8s-mount-propagation]

Concretely, this is a strictly larger grant than FUSE itself needs. Outside k8s, this repo's own
top-level `README.md` runs gostream with `--device /dev/fuse --cap-add SYS_ADMIN --cap-add NET_ADMIN`
— no `--privileged` — and only calls full `--privileged` an optional simplification "on a Raspberry Pi
where the container is fully trusted." **In k8s there is no equivalent middle ground**: Kubernetes
gates `mountPropagation: Bidirectional` on `securityContext.privileged: true` regardless of whether the
workload's own capability needs (`SYS_ADMIN` + `/dev/fuse`) are narrower. Privileged disables most
container isolation (all Linux capabilities, no seccomp/AppArmor confinement, unrestricted device
cgroup access) — the FUSE mount itself does not need that breadth; k8s's propagation model is what
requires it.

Consequences worth recording as explicit deployment prerequisites, not implementation details:

- **Pod Security Standards.** The `Baseline` and `Restricted` [Pod Security Standards][k8s-pss] profiles
  both list `spec.containers[*].securityContext.privileged` as a restricted field whose only allowed
  values are "Undefined/nil" or `false` — a Baseline/Restricted-enforcing namespace will reject this
  Pod outright. It can only run under the `Privileged` PSS profile (namespace label
  `pod-security.kubernetes.io/enforce: privileged`) or an equivalent, explicitly documented admission
  exception. flux's blue/green app tree needs to grant this namespace-level exception; it is not
  optional configuration this design can route around.
- **Blast radius is scoped to gostream's own container, not the Pod.** Only the gostream container
  carries `privileged: true`; jellyfin's container in the same Pod stays unprivileged
  (`privileged: false`, `allowPrivilegeEscalation: false`) with a read-only, receive-only
  (`HostToContainer`) mount of the same volume. Compromising jellyfin does not grant the host access
  gostream's side has; the privileged grant does not leak pod-wide for free.
- **Deferred, not built here:** a rootless/userns FUSE mount, or a CSI driver that performs the FUSE
  mount from a privileged node-level DaemonSet instead of an in-Pod privileged container, would shrink
  this blast radius further. Recorded as a future hardening direction, not implemented — no such driver
  exists for gostream's FUSE layer today and building one is out of this task's scope.

### SMB/CSI: named, untested, explicit non-default

An SMB/CIFS network share — via an operator-named SMB CSI driver (e.g.
[`csi-driver-smb`][csi-driver-smb]) backing a PersistentVolume instead of mount propagation — is a
real alternative for connecting jellyfin to gostream's virtual files, and this repo already has prior
art showing what it costs: the documented Raspberry Pi deployment in the top-level `README.md` runs
FUSE → **Samba** (`smbd`, `oplocks = no`, `vfs objects = fileid`) → a Synology CIFS mount
(`serverino, vers=3.0`) → Plex/Jellyfin. That extra hop exists there only because Plex/Jellyfin
historically ran on *separate* hardware from gostream; it papers over SMB's own client-side inode
handling with `serverino` + `vfs objects = fileid` tuning specifically so the mount doesn't fight
gostream's persisted inode map.

**This design records SMB/CSI as an explicit, untested non-default — it is not silently adopted.**
Reasons:

- Co-locating gostream and jellyfin in one Pod removes the reason SMB existed in the Pi setup (separate
  hardware); a same-node, same-Pod hop is exactly what mount propagation serves via the kernel, with no
  extra process.
- SMB/CIFS's client-side inode/identity handling is a second, independent mechanism layered on top of
  gostream's own persisted inode map (`gostream.db`) — an added moving part with its own tuning burden,
  precisely what mount propagation avoids.
- It would need its own CSI driver deployment plus an SMB server (either a Samba sidecar or exposing
  `smbd` from the gostream container) — nothing here has deployed or measured that path in-cluster.
- It reintroduces a network protocol hop for a same-node data path that the kernel already serves for
  free via propagation.

If a concrete future requirement needs network-share semantics — e.g. exposing the library to a media
client *outside* this Pod or cluster — SMB/CSI remains the documented escape hatch, but must be
separately validated (throughput, inode stability under gostream's `serverino`-style tuning) before
being adopted. It is not the reference default and this task does not validate it.

### Reference Pod fragment

`deploy/k8s/pod-fuse-fragment.yaml` — the shared `virtual-mkv` volume, gostream's `Bidirectional` +
`privileged: true` + explicit `/dev/fuse` `CharDevice` mount, and jellyfin's `HostToContainer` read-only
mount, as a syntactically valid (but deliberately incomplete) Pod. It is structurally sanity-checked
with:

```console
$ kubectl apply --dry-run=client --validate=strict -f deploy/k8s/pod-fuse-fragment.yaml
pod/gostream-fuse-reference created (dry run)
```

`--dry-run=client` never contacts an API server — it only confirms the object is well-formed against
kubectl's built-in schema (field names/nesting/types); it does not by itself prove the
`Bidirectional`/`HostToContainer`/`DirectoryOrCreate`/`CharDevice` string values are the correct ones —
those are drawn directly from the Kubernetes documentation cited throughout this file. Live propagation
correctness (does a FUSE entry gostream creates actually appear inside the jellyfin container on a real
cluster?) is **not** claimed here; see "Cross-references."

The fragment intentionally omits ports, env, probes, resource requests/limits, and the state/config
mounts — those belong to `k8s-reference-manifests` (composing this fragment into the full
Deployment/Pod template), `state-pvc`, and `config-secret`. Image references in the fragment are
explicit `REPLACE_ME` placeholders owned by the `k8s-image` and jellyfin `container-image` tasks
respectively — this task does not decide image tags.

### Cross-references

- **jellyfin's `colocation-persistence-contract` task** owns jellyfin's own side of this mirror
  contract: its `HostToContainer` read-only mount (recorded here as the counterpart to gostream's
  `Bidirectional` side), plus jellyfin's own `/var/lib/jellyfin` + `/etc/jellyfin` persistence and its
  blue/green library-sharing decision. See `submodules/jellyfin/INFRASTRUCTURE.md` and
  `submodules/jellyfin/docs/tasks/colocation-persistence-contract.md` in the beehive layer.
- **`k8s-reference-manifests`** composes this fragment into the full reference Deployment/Pod +
  Service (ports `:8080`/`:8090`/`:9080`, probes, GOMEMLIMIT/resources, the `state-pvc` and
  `config-secret` mounts).
- **flux's `phantom-library-bluegreen-deploy`** performs the actual live in-cluster bring-up and is
  where propagation is verified for real (a FUSE entry created by gostream observed inside the running
  jellyfin container) — cross-referenced here; **no live pass is claimed by this task.**

[k8s-mount-propagation]: https://kubernetes.io/docs/concepts/storage/volumes/#mount-propagation
[k8s-issue-95049]: https://github.com/kubernetes/kubernetes/issues/95049
[k8s-pss]: https://kubernetes.io/docs/concepts/security/pod-security-standards/
[csi-driver-smb]: https://github.com/kubernetes-csi/csi-driver-smb
