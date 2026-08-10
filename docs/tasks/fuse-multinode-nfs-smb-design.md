# Task: fuse-multinode-nfs-smb-design — network-export design for a future second node

Design-record ONLY. Priority 5 / future, LOW PRIORITY, gated on an actual second cluster
node existing. Today's cluster is single-node (`spray`); the mount-**propagation**
pairing in `fuse-mount-propagation` / `k8s-reference-manifests` (co-located gostream +
jellyfin sharing a host-mount-namespace `hostPath`) is sufficient and remains the
default for as long as that holds. No code, manifest, or live-cluster change ships with
this task — it exists so that, the day a second node is provisioned, the swarm has an
already-thought-through direction instead of re-deriving one under time pressure.

## Why propagation stops working with a second node

Mount propagation (`Bidirectional`/`HostToContainer`, `docs/tasks/fuse-mount-propagation.md`)
is a **host-mount-namespace** mechanism: it only reaches containers scheduled on the
SAME node as the FUSE mount's producer. A remote jellyfin pod scheduled onto the second
node has no mount-namespace path back to gostream's FUSE tree — propagation is
node-local by construction, not a k8s-level abstraction. Once a second node exists and
jellyfin (or any other consumer) may land there, the shared tree must become a real
network export.

## Direction: single-instance gostream, network-exported FUSE tree

- **gostream stays single-instance.** It is not distributed/sharded across nodes —
  there is exactly one gostream, on one node, owning one FUSE virtual-mkv tree. This is
  unchanged from today; multi-node changes ONLY how *consumers* reach that tree, not
  how many gostreams run.
- **Co-locate a network file server with gostream, not with the consumer.** `smbd`
  (Samba) or an NFS server (`nfs-kernel-server` / `nfs-ganesha`) runs in the same Pod (or
  a sidecar) as gostream, bind-mounted onto the SAME `allow_other` FUSE tree
  (`/mnt/gostream-mkv-virtual`) that propagation already reads from — no new mount
  path, just a new consumer of the existing `AllowOther: true` FUSE mount
  (`fuse-mount-propagation.md`).
- **Remote jellyfin pods consume it over the network**, not via `hostPath`/propagation:
  - **SMB**: a CSI SMB driver (`csi-driver-smb`) mounts the co-located `smbd`'s share
    into the jellyfin pod, wherever it is scheduled. This is the SAME `csi-driver-smb`
    option already named as an "explicit UNTESTED non-default" fallback in
    `fuse-mount-propagation.md` / `deploy/k8s/README.md` — this task's arrival (a real
    second node) is precisely the trigger that promotes it from documented fallback to
    the sanctioned multi-node path. It supersedes the older "operator does not use SMB"
    stance recorded elsewhere: SMB (or NFS) is now the accepted multi-node direction,
    strictly gated behind an actual second node existing.
  - **NFS**: an in-tree/CSI NFS mount (`nfs` volume type, or `csi-driver-nfs`) reaches
    the same co-located export. Simpler server-side (kernel NFS, no SMB auth/credential
    plumbing), POSIX-native semantics match a FUSE-backed tree more directly than
    SMB's.
- Single-node `spray` today needs neither: `hostPath` propagation is co-located,
  zero-network-hop, and already proven. Do not adopt SMB/NFS pre-emptively.

## Benchmark obligation before implementation

**Before actually implementing this** (i.e., when a second node is provisioned and this
task is picked up for real code), benchmark SMB vs NFS **sequential-read throughput**
against the FUSE virtual-mkv tree, under a realistic streaming access pattern (large
sequential reads, the video-serving workload gostream exists for) and pick the winner
by measured data — not by a priori preference. Candidate approach: `fio` or `dd`
sequential-read against a representative mounted file on each export, from a
remote-node consumer, several times to average out cache effects; compare achieved
MB/s and P50/P99 latency. Whichever wins becomes the implementation's transport; the
loser stays documented as the evaluated-and-rejected alternative (mirroring how
`fuse-mount-propagation.md` already documents SMB/CSI as a considered-but-not-chosen
option for the single-node case).

## Explicit non-goals of this design task

- No manifest, Dockerfile, or code change ships here — this is prose only.
- No SMB/NFS server is deployed, no CSI driver is installed, no benchmark is actually
  run — all deferred to the (currently nonexistent) second-node implementation task.
- Does not change the single-node default: `fuse-mount-propagation.md` /
  `k8s-reference-manifests` mount-propagation pairing remains authoritative until a
  second node exists.

## Trigger / follow-up

When an actual second cluster node is provisioned, file a real implementation task
(in this submodule) that: (a) stands up the co-located smbd/NFS server sidecar on the
FUSE `allow_other` tree, (b) runs the SMB-vs-NFS sequential-read benchmark above and
records the result, (c) wires the CSI driver (SMB or NFS, per the benchmark) into
remote jellyfin pod manifests, and (d) updates `deploy/k8s/README.md`'s "SMB / CSI"
section to reflect the now-sanctioned default instead of an untested fallback. This
design doc's job ends at handing that future task an already-reasoned starting point.

## Definition of done

`check=none`: no second node exists yet, so there is no observable effect to check —
a live SMB/NFS export, a CSI mount, or a benchmark result would all be premature and
untestable against a topology that does not exist. A reviewer judges this document's
completeness (does it correctly identify why propagation breaks at 2 nodes, does it
name a concrete direction, does it commit to a benchmark instead of guessing) rather
than running a check.
