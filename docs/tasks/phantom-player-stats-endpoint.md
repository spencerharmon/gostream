# Player stream statistics

`GET /api/library/stats?hash=<40-hex-v1-infohash>` on the existing metrics/library
HTTP listener. Use `X-Gostream-Token` with `library_api_token`, exactly as for
`/api/library/add`. When that token is unset, only the socket's loopback remote
address is allowed (forwarded headers do not grant access). Responses are JSON
and `Cache-Control: no-store`. Poll every 2–5 seconds; polling does not wake a
torrent, refresh its expiry, access the DB, or refresh validation leases.

## Response (200)

All fields below are always present. Unknown measurements are **null**, never a
synthetic zero. A measured zero (e.g. no connected peers) really is zero.

| Field | Type / meaning |
| --- | --- |
| `hash` | Lowercase hex of the requested loaded torrent |
| `peers` | Integer/null: engine `ActivePeers` (connected, not tracker swarm estimates) |
| `seeders` | Integer/null: engine `ConnectedSeeders` |
| `download_rate_bytes_per_sec` | Number/null: GoStorm's receive-byte delta / elapsed engine tick time, including protocol overhead; not media bitrate |
| `bytes_completed` | Integer/null: engine `BytesCompleted()`, not lifetime received/read bytes; may fall on RAM-cache eviction |
| `size` | Integer/null: **whole torrent** length, null until metadata arrives |
| `read_offset` | Integer/null: selected FUSE handle's **file-relative** cursor; pending read's start, otherwise end of its latest successful read |
| `buffered_ahead_bytes` | Integer/null: contiguous coverage starting at that cursor, clipped at file EOF; measured zero at a gap or EOF |
| `buffered_ahead_seconds` | Number/null: buffered bytes / file-specific bitrate in bytes/sec, when available; see limitation below |
| `stream_state` | `warmup`, `streaming`, `stalled`, or `idle`, defined below |
| `last_read_at` | RFC3339 timestamp/null: last successful nonempty **FUSE** read on the selected active handle, not seek time, attempted read time, or prefetch-pump time |
| `torrent_sampled_at` | RFC3339 timestamp/null: engine counter sample time |
| `stream_sampled_at` | RFC3339 timestamp/null: FUSE cursor/buffer sample time |

Buffer coverage uses recorded contiguous SSD **head** coverage (when present)
plus adjoining RAM read-ahead chunks. It stops at the first hole; neither total
cache allocation, far-ahead chunks, nor readahead target capacity count as
buffered data. Separate SSD tail-probe data and remote peer availability are not
counted. No payload copies or LRU access-time changes are made by sampling.

**Seconds are currently null in production.** The FUSE path does not retain a
trustworthy file-specific media duration/bitrate. The source interface supports
that measurement and the conversion is tested, but a future duration provider
must supply it. Never substitute torrent download speed, bursty FUSE read
throughput, whole-torrent size, or a hardcoded movie bitrate. The endpoint does
not launch ffprobe to answer a poll.

## State thresholds

These describe server-side read activity, **not** a Jellyfin pause/play flag.
Evaluate in this precedence order at `stream_sampled_at`:

1. **idle**: no sampled active FUSE handle for this hash.
2. **stalled**: the selected handle's latest requested read is still pending
   and has been waiting **at least 10 seconds** (also applies to the first read).
3. **idle**: no pending read and no new read request for **at least 30 seconds**;
   if no read was ever requested, use the handle's open time.
4. **warmup**: an active, non-idle handle has not returned any nonempty read yet.
5. **streaming**: remaining active handles (including brief waits <10 seconds).

A cached stream can be streaming with zero download rate. A paused or buffered
player can be idle. Downloads and the native prefetch pump alone do not imply
playback. Short stalls between samples may not be observed. Samples are not
extrapolated into a stall after their observation time: clients should use the
sample timestamps to distinguish an old observation from a fresh one.

For multiple players/files/handles sharing a hash, select the **most recently
requested** active handle (open time for a handle with no reads). Never sum their
buffers or pretend a hash-only API identifies a particular player. On release
that handle disappears on the next sample; no active handle means nullable read
fields, not persisted historical `last_read_at`. Scanners also open FUSE handles
and can therefore appear here; no player-identity claim is made.

## Sampling, complexity, errors

- Engine counters reuse the existing **1-second BTServer tick** and are published
  as immutable samples. Before the first tick, all counter fields are null.
- FUSE sampling runs in the existing monitor collector cycle (nominally **5s**).
  That cycle also polls other services and can be delayed by their timeouts; use
  `stream_sampled_at` rather than assuming every HTTP poll is a new sample.
- The request performs one live-torrent map lookup, one atomic counter-sample
  load, and one collector map lookup: **O(1)** in torrent/file/peer/chunk counts.
  No additional background loops. Buffer walks happen only during sampling.
- `400 invalid_infohash`: absent, repeated, non-40-character, or nonhex hash.
  Uppercase hex is accepted and normalized. Magnets/base32 are not accepted.
- `401 missing_or_invalid_token` / `403 loopback_required_when_token_unset`: same
  authorization as Add, checked before method/hash/engine lookup.
- `404 torrent_not_found`: hash is not loaded in the in-process engine. This
  includes expired/removed torrents and DB-only dormant entries; polling does
  **not** resurrect them. Unknown and removed hashes cannot return cached stats.
- `405 method_not_allowed`, `Allow: GET`: other methods.

## Tests and release

`go test ./internal/monitor/dashboard/...` covers fake-engine state boundaries,
null preservation, normalization, unknown hash and auth. Additional tests in
`internal/monitor/collector`, `internal/gostorm/torr`, and the root package cover
read bookkeeping, indexed sample replacement, real metadata-free/metadata-ready
engine snapshots, route wiring, FUSE cache-hit reads, and contiguous-buffer gaps.

No listener, secret, deployment manifest, or build workflow changes. Image
rebuild/publish and deployment use the existing gostream image pipeline after
merge; this code change does not claim an already deployed endpoint.
