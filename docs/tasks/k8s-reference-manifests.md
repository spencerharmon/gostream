# k8s-reference-manifests — reference Deployment + Service (design)

Integration capstone (ROI Priority 3). Composes the four deps
(`fuse-mount-propagation`, `state-pvc`, `config-secret`, `healthz-probe`) into the
single-color **reference** gostream spec that flux's `phantom-library-bluegreen-deploy`
consumes and adapts. Sibling of `k8s-image` (shares the image contract in
`ARTIFACTS.md`). This doc is the code-side rationale; operator-facing compose notes live
in `deploy/k8s/README.md`.

## What shipped (`repo/deploy/k8s/`)

- **`deployment.yaml`** (new) — single-color gostream `Deployment` (`Recreate` strategy,
  1 replica). Carries: `securityContext.privileged: true`; the `Bidirectional`
  `virtual-mkv` FUSE mount; the `gostream-state` PVC at `/usr/local/state`; the split
  ConfigMap+Secret merged to `/etc/gostream/config.json`; `/healthz`+`/readyz` probes on
  `:9080`; `GOMEMLIMIT=2200MiB` + CPU/memory requests+limits; container ports
  8080/9080/8090; `terminationGracePeriodSeconds: 60` so gostream saves the inode map +
  unmounts FUSE on TERM.
- **`service.yaml`** (new) — `ClusterIP` `Service` fronting `:8080`/`:9080`/`:8090` via
  named `targetPort`s; single-color selector flux flips per color.
- **`kustomization.yaml`** (new) — composes the single-color base
  (`deployment.yaml` + `service.yaml` + `configmap.yaml`) so `kubectl kustomize
  deploy/k8s` builds it. Deliberately excludes the schema-only Secret, the state PVC
  (shipped by `state-pvc`), the reference-only fuse fragment, and flux's jellyfin/color
  overlay.
- **`README.md`** — new "Reference Deployment/Service (the capstone)", "Networking:
  ClusterIP Service, not hostNetwork", resource/GOMEMLIMIT, and privileged-tradeoff
  recap sections; intro piece-list updated.

## Decisions (the accept criteria, point by point)

- **Networking — ClusterIP Service, NOT hostNetwork.** Blue/green needs two colors
  co-resident on the single node during cutover; `hostNetwork` puts both in the node
  netns so they collide on every host port (`:8080`/`:9080`/`:8090` + the torrent port),
  breaking the model this reference feeds. A ClusterIP Service gives each color its own
  Pod IP and lets flux flip the selector to cut over. Recorded in `service.yaml` +
  README + `INFRASTRUCTURE.md`.
- **Peer/torrent inbound + NAT-PMP is nft-only.** Handled by gostream's built-in NAT-PMP
  (`config natpmp.*`, default off), not hostNetwork. Its port-map/firewall work uses the
  **nft/iptables-nft** backend only — the image's bookworm `iptables` is `iptables-nft`
  by default, so gostream's `sudo iptables` calls already run through nftables; no
  legacy-iptables tooling is added or depended on. `CAP_NET_ADMIN` comes from
  `privileged`. A fixed inbound port without NAT-PMP is a per-color `hostPort`/NodePort
  for just the torrent port — never a whole-pod hostNetwork switch.
- **Privileged FUSE.** `privileged: true` is required (kubelet allows `Bidirectional`
  only for privileged; it also supplies `/dev/fuse`+`SYS_ADMIN`+`NET_ADMIN`). Bounded:
  only gostream privileged; the jellyfin sidecar flux adds is unprivileged+RO; scoped
  hostPath; single node. Drop-privileged (device plugin + caps) loses `Bidirectional`
  → SMB/CSI; recorded as non-default.
- **State at `/usr/local/state`, no db wipe.** `GOSTREAM_ROOT_PATH=/usr/local/state`
  equals the `gostream-state` PVC mountPath, so `gostream.db` (`GetStateDir()` =
  `$ROOT_PATH/STATE`) persists. No init/job wipes it. PVC object stays owned by
  `state-pvc`; the Deployment references the claim by name.
- **Merged config at `/etc/gostream/config.json`.** `MKV_PROXY_CONFIG_PATH` +
  `*_TUNING_PATH`/`*_SECRET_PATH` drive the entrypoint's jq deep-merge; writable
  `config-merged` emptyDir + RO ConfigMap/Secret sources.
- **Probes on `:9080`.** `/healthz` liveness (lenient), `/readyz` readiness (tolerant
  startup window per the boot-order note: `:9080` answers before `fs.Mount()` runs).
- **GOMEMLIMIT + limits.** `2200MiB` soft ceiling below the 3Gi hard limit (headroom for
  cgo/FUSE off-heap); requests 500m/1Gi, limits 2/3Gi.
- **Three ports exposed** via the Service (8080 inert-but-contractual, 9080 real, 8090
  GoStorm all-interfaces → NetworkPolicy note).
- **Single-color reference only.** No jellyfin sidecar, no color overlay here — flux's
  job, cross-referenced.

## Ground-truth facts reused (not re-derived)

Ports/bind-addresses, PID1/clean-TERM, GOMEMLIMIT default, `GOSTREAM_ROOT_PATH` vs
`/usr/local/state`, and the fail-loud `/dev/fuse`+`SYS_ADMIN` gate come verbatim from
`submodules/gostream/ARTIFACTS.md` (verified by `k8s-image`) and the entrypoint/env
contract in `docker/docker-entrypoint.sh`. FUSE propagation pairing + hostPath choice
come from `pod-fuse-fragment.yaml` / the `fuse-mount-propagation` doc. The state
contract + shared-claim blue/green decision come from the `state-pvc` doc. The probe
paths/semantics come from the `healthz-probe` doc.

## Validation (client-side, offline — no cluster)

- `kubectl kustomize deploy/k8s` → builds ConfigMap + Service + Deployment cleanly.
- `kubectl create --dry-run=client -f deploy/k8s/deployment.yaml -o name` →
  `deployment.apps/gostream`; `-f service.yaml` → `service/gostream`.
- Composed output asserted to carry: `privileged: true`, `mountPropagation:
  Bidirectional`, `/usr/local/state`, `/etc/gostream/config.json`, `/healthz`+`/readyz`
  on `:9080`, `GOMEMLIMIT`, resource `limits`, `claimName: gostream-state`, and all three
  container+service ports; parses as 3 valid docs.

## Deferred to flux (cross-referenced, NOT claimed here)

Live in-cluster bring-up, propagation verification, the real patched jellyfin sidecar,
the SOPS Secret, the blue/green color overlay, and the `spray` nodeSelector are flux's
`phantom-library-bluegreen-deploy`. This task ships the reference spec only.
