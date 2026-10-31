# Handing downloaded images to Docker

Status: decided for Milestone 1. Revisit if the trade-offs below change.

ddm downloads every blob itself, then has to give the finished image to Docker
without Docker touching the network. There were two candidates.

## Option A: stream an archive into `docker load` (chosen)

ddm writes a tar archive from the cached blobs straight into the stdin of
`docker load`. Nothing touches the disk on the way. The archive uses the same
layout `docker save` has produced since Docker 25:

```
oci-layout
index.json              OCI index → the original manifest, named via
                        io.containerd.image.name / org.opencontainers.image.ref.name
manifest.json           legacy docker-archive manifest (Config, RepoTags, Layers)
blobs/sha256/<config>
blobs/sha256/<layer>…   original compressed layers, unchanged
blobs/sha256/<manifest> original manifest bytes
```

- The **classic image store** (graph drivers, the default on Docker Engine for
  Linux) reads `manifest.json`. It decompresses each layer, checks it against
  the config's `diff_ids`, and tags the image only at the end.
- The **containerd image store** (the default in current Docker Desktop) reads
  `index.json`. It imports the original manifest, so the image keeps its
  upstream manifest digest.

## Option B: a temporary registry on 127.0.0.1

ddm would serve the cache as a registry on a loopback port, run
`docker pull 127.0.0.1:PORT/name:tag`, re-tag the image to its original name,
and remove the temporary tag.

## Why A

| Concern | A: `docker load` | B: local registry |
|---|---|---|
| Daemon on another host or VM (Docker Desktop on macOS/Windows, WSL2, `DOCKER_HOST=ssh://…`, remote contexts) | Works. The tar goes over the same API connection the CLI already uses. | Fails or needs extra work. `127.0.0.1` is the daemon's loopback, not ours. On Docker Desktop the host is only reachable as `host.docker.internal`, which is not an insecure registry by default. |
| Rootless Docker | Works | `127.0.0.1` sits in RootlessKit's network namespace |
| Daemon configuration | None | Relies on loopback being treated as an insecure registry. Fragile with registry mirrors and proxies set in `daemon.json`. |
| Atomicity | Docker registers and tags the image only after the whole archive is read and verified. If the stream breaks part-way, nothing is imported. | `docker pull` is atomic too, but the re-tag/untag dance afterwards is not. A crash leaves a stray `127.0.0.1:PORT/...` tag. |
| Final name | Set directly (RepoTags / index annotations) | Needs re-tag plus cleanup |
| Ports, firewalls, AV | None | Opens a listening socket, and corporate endpoint security sometimes blocks it |
| Layers Docker already has | Streamed again (local I/O only, no network) | Skipped by Docker |
| `RepoDigests` (classic store) | Not filled in | Filled in (for the temporary name) |

Option B's real advantages are skipping layers Docker already has and filling
in RepoDigests. Both are cosmetic or local-I/O costs. Option A works the same
everywhere Docker runs, which is what a tool for unreliable environments
needs most.

## Atomicity details

- An import only starts after every blob is in the cache with a verified
  digest.
- Each blob is hashed again while it streams into the archive. If cached data
  has rotted, ddm aborts the stream and Docker rejects the truncated archive.
  ddm then deletes the bad blob so that the next run downloads it again.
- Ctrl+C during the import kills `docker load`, and Docker throws away what it
  had read so far.

## Known differences from a plain `docker pull`

1. **Classic store:** `docker images --digests` shows `<none>` for images
   imported by ddm, because `docker load` does not record RepoDigests.
   Running containers and tags work normally.
2. **containerd store, multi-platform images:** the image ID is the digest of
   the platform-specific manifest, not the index digest that `docker pull`
   would record. ddm counts either one as "up to date".
3. **Pull by digest** (`name@sha256:…`) loads an untagged image on the
   classic store, because RepoTags cannot hold a digest reference. A follow-up
   could tag it as `name:<short-digest>` or record the digest some other way.
4. **Layers Docker already has** are still downloaded unless they are in the
   ddm cache, which is wasted bandwidth on a slow link. Possible follow-up for
   the classic store: `docker load` skips any layer whose chain ID already
   exists without reading it, so ddm could leave those layers out of the
   archive. Docker's existing chain IDs can be computed from
   `docker image inspect` (`RootFS.Layers`). This needs real-Docker testing on
   both stores before it is enabled.
