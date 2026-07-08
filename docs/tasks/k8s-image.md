# Task: k8s-image — tune the gostream image for clean k8s deployment

Tunes the existing `docker.io/mrrobotogit/gostream:testing` image
(`docker/Dockerfile` + `docker/docker-entrypoint.sh`) to deploy cleanly under
k8s without regressing `:testing`'s streaming patches. Parallel sibling of
`k8s-reference-manifests` — same image contract (ports, env, entrypoint), no
k8s manifests ship from this task.

## Changes

- **`docker/Dockerfile`**:
  - `EXPOSE 8080 8090 9080` (was missing `8080`) — documents the full port
    contract even though `8080` (`proxy_listen_port`) is not currently bound to
    any listener in the Go code (see ARTIFACTS.md; out of scope to fix here).
  - `ENV GOMEMLIMIT=2200MiB` — carries `gostream.service`'s systemd default
    (`Environment=GOMEMLIMIT=2200MiB`) into the image. A plain image `ENV` is a
    default, not a ceiling: any container/pod-level `env:` of the same name
    overrides it, verified with `-e GOMEMLIMIT=512MiB`.
- **`docker/docker-entrypoint.sh`**:
  - New `require_fuse()` gate, run first, before touching config/state: fails
    loud (exit 1, operator-actionable message naming the exact Docker
    flag/k8s `securityContext` field) if `/dev/fuse` is missing, or if
    `CAP_SYS_ADMIN` is absent from `/proc/self/status`'s `CapEff` (bit 21,
    mask `0x200000` — covers `--cap-add SYS_ADMIN` and `--privileged`, both
    verified to set the bit). Without this, a missing device/cap only surfaced
    once `fs.Mount()` failed deep inside gostream startup (`main.go`'s
    `log.Fatal(err)`), after state/log dirs and the config merge had already
    run.
  - Fixed the shutdown/signal-relay logic (see "Shutdown bug" below) so the
    container's exit code reflects gostream's real exit status instead of an
    artifact of the shell's own signal handling.

## Shutdown bug found + fixed (dash `trap`/`wait` interaction)

Investigating the PID 1 / clean-TERM-unmount accept criterion surfaced a real,
reproducible bug in the pre-existing shutdown logic, isolated with a synthetic
Go binary that mimics gostream's signal-handling shape (catches TERM, sleeps
N seconds to simulate graceful work, exits with a configurable code) under the
exact same `tini` + entrypoint wrapper:

- The old `shutdown()` trap handler did the real `kill -TERM "$gostream_pid"`
  + `wait` + `fusermount3` work, then fell through to a second, now-dead copy
  of the same steps in the main script body (which, empirically, **never
  executes** — once a trapped signal interrupts a `wait`, dash does not resume
  the interrupted call or its following lines; it ends the shell right after
  the trap handler returns, using the signal's `128+n` status). Net effect:
  the container's reported exit code was **always `143`** on TERM, regardless
  of whether gostream itself exited `0` or something else — a real status was
  silently discarded, not just a cosmetic wart, since `143` is indistinguishable
  from "gostream itself died to an uncaught signal."
- Fix: the trap handler now owns the *entire* reap — it `wait`s for the real
  PID, captures the real code with `set +e`/`set -e` bracketing (a bare
  `set -e` script aborts on a non-zero `wait` *before* `code=$?` can even run,
  which would have skipped both the `fusermount3` safety net and the exit call
  for any non-clean gostream exit), then calls `exit "$code"` itself, since
  nothing after the interrupted `wait` in the main flow will ever run anyway.
- Verified matrix (synthetic binary, signaling the container's host-visible
  PID 1 directly, not just relying on `podman stop`'s own timeout dance):

  | Scenario | Expected | Result |
  |---|---|---|
  | TERM, graceful 2s cleanup, exit 0 | full 2s honored, exit `0` | ✅ |
  | TERM, graceful 2s cleanup, exit 9 (simulated error) | full 2s honored, exit `9` | ✅ |
  | No signal, self-exit 7 (simulated crash) | exit `7`, `fusermount3` still runs | ✅ |

- Re-verified end-to-end against the **real** gostream binary (podman,
  `--device /dev/fuse --cap-add SYS_ADMIN`, minimal `config.json.example`):
  container exit code is now `0` on TERM (previously `143`), confirming the
  entrypoint/PID1 layer correctly waits for and reports gostream's true exit.

### Related, NOT fixed here (out of file scope — `main.go`)

Even with the shell fix, gostream's own SIGTERM goroutine's last two
statements (a confirmation log + an explicit `os.Exit(0)`) don't reliably run:
`main()`'s own last statement, `server.Wait()`, unblocks as soon as the SAME
goroutine's `server.Unmount()` call (a few lines earlier) succeeds, and in
every local repro `main()` returning (implicit exit) wins the race before the
goroutine reaches its last two lines. This is benign for correctness — the
inode-map save, sync-cache save, and the unmount itself all happen *before*
the race point, so "inode-map stability is sacred" is not at risk, and the
process exit code is `0` either way — but it means that log line is
unreliable. Recorded in `ARTIFACTS.md` for whoever next touches `main.go`'s
shutdown path; not a `k8s-image` blocker since this task's own file scope
(`docker/Dockerfile`, `docker/docker-entrypoint.sh`) is where the fix belongs
and the entrypoint/PID1 contract is independently verified correct.

## Files

- `docker/Dockerfile` — `EXPOSE 8080 8090 9080`, `ENV GOMEMLIMIT=2200MiB`.
- `docker/docker-entrypoint.sh` — `require_fuse()` fail-loud gate; corrected
  shutdown/signal-relay logic (true exit code, `set -e`-safe).
- `ARTIFACTS.md` (beehive layer) — ground-truth port/PID1/GOMEMLIMIT/config
  facts for `healthz-probe` and `k8s-reference-manifests` to reuse.

## Validation

- `podman build` from a clean worktree: reproducible.
- `gofmt -l .` empty, `CGO_ENABLED=1 go vet ./...` clean, `CGO_ENABLED=1 go
  build ./...` clean, `CGO_ENABLED=1 go test -count=1 ./...` all `ok` — run
  inside the pinned `golang:1.24-bookworm` image (matches `zuul-ci`), not just
  the host toolchain.
- Fail-loud matrix: no `/dev/fuse` → fails naming the Docker/k8s fix; `/dev/fuse`
  present, no `CAP_SYS_ADMIN` → fails naming the cap fix; both present →
  gostream starts and FUSE-mounts successfully.
- `GOMEMLIMIT` present by default (`2200MiB`) and overridable via `-e`/pod env.
- Config-secret's split-config merge (tuning + secret → one `config.json`,
  `plex` object recombined) re-verified working unchanged with `require_fuse()`
  inserted ahead of it in the entrypoint.
- Shutdown fix verified per the matrix above, plus end-to-end against the real
  binary (exit code `0` on TERM, was `143`).

No k8s manifests ship from this task — pod wiring is `k8s-reference-manifests`.
