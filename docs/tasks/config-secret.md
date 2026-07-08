# Task: config-secret — split config.json into ConfigMap + Secret

Split gostream's single `/etc/gostream/config.json` (env `MKV_PROXY_CONFIG_PATH`)
into a non-secret **ConfigMap** and a **Secret**, merged back to one file at
container start. Deliverables live in `deploy/k8s/` and `docker/`.

## Key split (from `config.json.example`, no invented keys)

- **Secret** (`config.secret.json`): `plex.token`, `tmdb_api_key`,
  `library_api_token` — the only credentials in the example.
- **ConfigMap** (`config.tuning.json`): everything else, including the non-secret
  `plex.url` and `plex.library_id`.
- `plex` is therefore a **split object**, which forces a *recursive* merge.
- `prowlarr.api_key` is a credential too but is **absent** from
  `config.json.example`, so it is not added; if Prowlarr is enabled later, add
  `prowlarr.api_key` to the Secret — the recursive merge handles the nesting.

## Merge mechanism: entrypoint jq deep-merge onto an emptyDir

`docker/docker-entrypoint.sh` deep-merges when both source env vars are set:

```sh
jq -s '.[0] * .[1]' "$MKV_PROXY_CONFIG_TUNING_PATH" \
                    "$MKV_PROXY_CONFIG_SECRET_PATH" > "$MKV_PROXY_CONFIG_PATH"
```

- `*` = recursive merge, secret wins → `plex{url,library_id}` + `plex{token}` =
  `plex{url,library_id,token}`. One valid `config.json`, same shape as the example.
- Atomic write (temp + `mv`); no secret values logged; guarded so a half-config
  (only one var) or a missing source **fails fast**.
- Unset vars → merge skipped → Docker/systemd single-file installs unchanged.
- Needs `jq`; added to `docker/Dockerfile` (runtime stage). Coordinated with
  `k8s-image`.

**Rejected:** projected volume (can't deep-merge a split object into one file);
standalone-image initContainer (official `ghcr.io/jqlang/jq` is shell-less so it
can't write the merged file; a deep merge needs `jq` in *some* image, so baking it
into the first-party gostream image is cleaner than a third-party `sh`+`jq` image).

## Secret handling (SOPS)

`deploy/k8s/secret.sops.example.yaml` is committed **schema-only** (empty tokens,
zero plaintext). The real Secret is **SOPS-encrypted in flux's tree**, decrypted
in-cluster. Never commit populated tokens to this repo.

## Files

- `deploy/k8s/configmap.yaml` — tuning ConfigMap (`gostream-config`).
- `deploy/k8s/secret.sops.example.yaml` — schema-only Secret (`gostream-config-secret`).
- `deploy/k8s/README.md` — split table, merge justification, SOPS, pod wiring, validation.
- `docker/docker-entrypoint.sh` — guarded jq deep-merge.
- `docker/Dockerfile` — adds `jq`.

## Validation

- `kubectl create --dry-run=client -f deploy/k8s/configmap.yaml -o name` → ok.
- `kubectl create --dry-run=client -f deploy/k8s/secret.sops.example.yaml -o name` → ok.
- Entrypoint merge exercised locally: deep merge correct, secret wins, guards
  reject half-config/missing-source, backward-compat path unchanged.

Pod wiring (Deployment/Pod, PVC, probes) is the `k8s-reference-manifests` capstone.
