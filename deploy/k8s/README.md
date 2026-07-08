# gostream on Kubernetes — split config (ConfigMap + Secret)

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
