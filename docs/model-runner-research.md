# Docker Model Runner: how models are stored and imported

Status: research for Milestone 4, **awaiting approval before the import step
is implemented**. Researched on 2026-10-31 against the `main` branch of
[docker/model-runner](https://github.com/docker/model-runner) and the
current Docker docs. Model Runner is still marked experimental, so the
internal API below can change. The "Risks" section covers how ddm should cope
with that.

## TL;DR

| | Docker Hub / OCI models (`ai/smollm2`, `registry.example.com/x`) | Hugging Face models (`hf.co/org/repo[:QUANT]`) |
|---|---|---|
| How the runner gets them today | OCI registry pull, with resumable Range requests | Downloads raw files from the Hugging Face Hub and builds an OCI artifact locally. **Not resumable, no checksum verification** |
| How ddm should download | The existing engine: manifest, config, layers by digest, mirrors | Hugging Face API to list files, then the existing engine on `resolve/` URLs, verified against each file's LFS sha256 |
| How ddm should import | **Upload a tar to the runner's `POST /models/load`**, keeping the original manifest so the model ID stays the same, then tag it | **`docker model package --gguf …`** (or `--safetensors-dir`) builds the artifact through the official CLI, then tag it |
| Name afterwards | `ai/smollm2:latest`, the same as `docker model pull` | `huggingface.co/org/repo:QUANT`, the same as `docker model pull` |

Both imports **push data into the runner** over its API. The runner never
connects back to ddm, so the "127.0.0.1 inside the runner container is not
the host" problem on Docker Engine doesn't come up.

## 1. What a Docker model is

Models are OCI artifacts in a registry, with their own media types
([MODEL_TYPES.md](https://github.com/docker/model-runner/blob/main/pkg/distribution/MODEL_TYPES.md)):

| Role | Media type |
|---|---|
| config (legacy, used for GGUF) | `application/vnd.docker.ai.model.config.v0.1+json` |
| config (one layer per file, used for safetensors) | `application/vnd.docker.ai.model.config.v0.2+json` |
| GGUF weights | `application/vnd.docker.ai.gguf.v3` |
| Safetensors weights | `application/vnd.docker.ai.safetensors` |
| License | `application/vnd.docker.ai.license` |
| CNCF ModelPack config / weights | `application/vnd.cncf.model.config.v1+json`, `application/vnd.cncf.model.weight.v1.{gguf,safetensors}` |

The manifest itself is a plain `application/vnd.oci.image.manifest.v1+json`.
Layers are **uncompressed**, so a layer's digest is the same as its diff ID.
The runner accepts config types v0.1, v0.2 and ModelPack v1, and rejects
anything else with `ErrUnsupportedMediaType`. **The model ID is the digest of
the raw manifest**, so importing the exact manifest bytes keeps the ID.

What this means for ddm:

- The download engine already handles these: a layer is a content-addressed
  blob, fetched with Range requests and verified by sha256.
- `ddm pull` already refuses model configs and points the user to
  `ddm model pull`.
- The only new piece is the import step.

## 2. Names

From `normalizeModelName` and its tests
([normalize_test.go](https://github.com/docker/model-runner/blob/main/pkg/distribution/distribution/normalize_test.go)):

| Input | Stored as |
|---|---|
| `gemma3` | `ai/gemma3:latest`. The default org is `ai/` on Docker Hub, and the name is lowercased |
| `myorg/model` | `myorg/model:latest` |
| `registry.example.com/myorg/model:v1` | unchanged |
| `hf.co/org/Model-GGUF` | `huggingface.co/org/Model-GGUF:latest`. **Case is kept** |
| `hf.co/org/Model-GGUF:Q4_K_M` | `huggingface.co/org/Model-GGUF:Q4_K_M` |

For Hugging Face references the tag is a **quantization hint**, not a
registry tag. `latest`, `main` or no tag selects the default quantization,
which the docs give as Q4_K_M, falling back to the first GGUF file.

## 3. How the runner pulls today

[`client.go`](https://github.com/docker/model-runner/blob/main/pkg/distribution/distribution/client.go):

- **OCI** (`PullModel`): fetches the manifest and checks the config type. For
  each layer it checks for an incomplete blob in its store and resumes it
  with Range requests (`remote.WithResumeOffsets`). It has its own
  `RegistryMirrors` and `PlainHTTP` client settings.
- **Hugging Face** (`pullNativeHuggingFace`, then `huggingface.BuildModel`):
  lists the repo's files, picks the weight files for the quantization
  (`SelectGGUFFiles`, which also picks the F16 `mmproj` for multimodal
  models), downloads them plus config and chat-template files to a temp
  directory, and builds the artifact:
  - GGUF: `builder.FromPaths` (config v0.1)
  - safetensors: `builder.FromDirectory` (config v0.2)

  The created time is the repo's `lastModified`, so the same revision gives
  the same digest. The
  [downloader](https://github.com/docker/model-runner/blob/main/pkg/distribution/huggingface/downloader.go)
  calls `os.Create` (truncating), copies the whole response, deletes the file
  on error, sends **no Range header**, and **verifies no checksum**. Four
  files download in parallel.

So a 20 GB Hugging Face model that drops at 19 GB starts again from zero.
That is the case ddm exists for.

## 4. Ways to get a model into the runner

### A. `POST /models/load` with a tar stream: recommended for OCI models

The CLI's own `docker model package` uses this route to load an artifact
locally
([package.go](https://github.com/docker/model-runner/blob/main/cmd/cli/commands/package.go),
[desktop.go](https://github.com/docker/model-runner/blob/main/cmd/cli/desktop/desktop.go)):

```
POST {base}/models/load
Content-Type: application/x-tar
<tar stream>
```

The server's `handleLoadModel` calls `Manager.Load`, which calls
`distribution.Client.LoadModel`. That reads the stream with
`tarball.NewReader`, writes each blob, and then writes the manifest **last**,
returning the model ID. The tar layout, from
[tarball/target.go](https://github.com/docker/model-runner/blob/main/pkg/distribution/tarball/target.go),
is:

```
blobs/
blobs/sha256/
blobs/sha256/<layer hex>     one per layer, in manifest order (named by diff ID, which is the digest for uncompressed model layers)
blobs/sha256/<config hex>
manifest.json                the raw manifest bytes
```

There is no tag in the tar. Tagging is a separate call:

```
POST {base}/models/{id}/tag?repo=<repo>&tag=<tag>      → 201 Created
```

Why this route:

- **The original manifest bytes are preserved**, so `docker model list` shows
  the same ID as a regular pull.
- **Every layer type is carried over unchanged**: license, chat template,
  mmproj, multiple safetensors files.
- **Data flows from ddm to the runner**, which works whether the runner is on
  the host (Desktop) or in a container (Engine).
- **Close to atomic.** Writing the manifest last means an interrupted load
  leaves orphan blobs but no visible model. ddm also verifies every blob
  before sending it, as it does for `docker load`.

### B. `docker model package`: recommended for Hugging Face models

```
docker model package --gguf /abs/first-shard.gguf [--mmproj f16.gguf] [--chat-template t.jinja] [--context-size N] MODEL
docker model package --safetensors-dir /abs/dir MODEL
```

Without `--push`, the CLI builds the artifact itself and streams it to
`/models/load` (option A), so this is still a push into the runner. The
official builder produces the config: format, quantization, parameter count,
architecture and size, read from the GGUF metadata. It also discovers GGUF
shards. ddm doesn't have to reimplement any of that.

Drawbacks:

- **The digest differs from a native pull.** The created time becomes the
  current time instead of Hugging Face's `lastModified`, unless the flags
  gain a way to set it. Functionally it's the same model.
- **The tag may be a problem.** The CLI parses `MODEL` with
  `reference.NewTag`, which rejects uppercase letters, but Hugging Face names
  keep their case (`huggingface.co/bartowski/Llama-3.2-1B-Instruct-GGUF`).
  Plan: package under a temporary lowercase tag, then tag with the exact
  name through `POST /models/{id}/tag`, then remove the temporary tag. Open
  question 1 asks whether the server accepts that name.

### C. Serve the model from a temporary local registry, then `POST /models/create`: rejected

- On Docker Engine the runner runs in a container, where `127.0.0.1` is the
  container, not the host.
- The runner would need `PlainHTTP` for a local registry.

These are the same reasons ddm uses `docker load` for images
([image-import.md](image-import.md)).

### D. Write into the runner's store directly: rejected

- The store layout is internal.
- On Engine it lives in a volume inside the runner container.
- A running runner would race with ddm.

## 5. Reaching the runner's API

ddm should copy the CLI's
[`DetectContext`](https://github.com/docker/model-runner/blob/main/cmd/cli/desktop/context.go)
order:

1. **`MODEL_RUNNER_HOST` is set:** use it as the base URL.
2. **Docker Desktop** (the engine reports `OperatingSystem == "Docker Desktop"`;
   on Linux only the WSL2 variant counts): the base is
   `http://localhost/exp/vDD4.40`, dialed **through the Docker engine
   socket**. That is the context's `unix://` socket, or `npipe://` on Windows.
   - ddm reads the endpoint from `docker context inspect`.
   - Windows named pipes need `github.com/Microsoft/go-winio`, which is a new
     dependency.
   - If the user turned on host-side TCP (`docker desktop enable
     model-runner --tcp 12434`), `http://localhost:12434` also works.
3. **Docker Engine (Moby)**: the standalone runner container publishes
   `http://localhost:12434` (12444 with TLS).
4. **Docker Cloud contexts** (ports 12435/12445): left out for now.

## 6. Prerequisite checks for `ddm docker model …`

| Check | How | If it fails |
|---|---|---|
| Plugin installed | `docker model version` exits 0 | Desktop: update Docker Desktop. Linux: `sudo apt-get install docker-model-plugin` or `sudo dnf install docker-model-plugin`. **Print only; never install packages.** |
| Runner enabled and running | `docker model status` | Desktop: Settings > AI > Enable Docker Model Runner, or **offer to run** `docker desktop enable model-runner` after asking. Engine: **offer to run** `docker model install-runner` after asking. ddm can pull the runner image with its own engine first, so this works on a bad connection too. |
| API reachable | `GET {base}/models` returns 200 | Show the base URL that was tried, and how to set `MODEL_RUNNER_HOST` |
| Model already present | `GET {base}/models/{name}` (200 or 404) | Download and import |
| Memory | Model size (sum of weight layers, or of the selected Hugging Face files) against host RAM (Linux `/proc/meminfo`, macOS `sysctl hw.memsize`, Windows `GlobalMemoryStatusEx`) and VRAM (`nvidia-smi --query-gpu=memory.total`, when available) | **Warn, don't block**, when the weights exceed about 70% of RAM (or of VRAM when a GPU backend is in use). The runner also needs room for the context. |

## 7. Plan for the implementation (after approval)

1. **`internal/modelrunner`** for the runner API:
   - context detection as in section 5
   - `List`, `Inspect`, `Load(tar)`, `Tag`, `Untag`
   - `Status`, `Version`
   - a tar writer for the layout in 4.A
2. **`internal/hf`** for Hugging Face:
   - list a repo's files with the Hub API, including each LFS sha256 and size
   - pick files the same way as `SelectGGUFFiles`: the quantization from the
     tag, defaulting to Q4_K_M, with sharded GGUF and the F16 mmproj
   - download URLs `{endpoint}/{repo}/resolve/{revision}/{path}`
   - `HF_TOKEN` as a bearer token
   - mirrors: `hf.co`/`huggingface.co` entries in the existing mirror config,
     e.g. `https://hf-mirror.com`, plus `HF_ENDPOINT`
3. **`ddm model pull MODEL`**:
   - OCI: the existing resolve and fetch, then import with A, then tag.
   - Hugging Face: list and select, fetch every file through the engine,
     verifying sha256 (and resuming with Range), then import with B.
4. **`ddm docker model run|pull …`** in pass-through:
   - check the prerequisites in section 6
   - download and import the model if it is missing
   - then exec the original command, as for images
5. **`ddm doctor`**: add the runner status, the endpoint that answered, and
   the list of models.
6. **Tests**:
   - a fake runner API (an `httptest` server that records the uploaded tar
     and tag calls)
   - a fake Hugging Face Hub, using the existing flaky server, with
     `api/models/...` and `resolve/` endpoints
   - quantization selection
   - the tar layout

## 8. Open questions to settle on a real install

1. Does `POST /models/{id}/tag` accept `huggingface.co/<Org>/<Repo-With-Caps>`?
   If not, Hugging Face imports would use path A with a config ddm builds
   itself. That needs a small GGUF metadata reader for the
   quantization/parameters/architecture fields. It's a fallback, not plan A.
2. Does `tarball.NewReader` verify blob digests? ddm verifies before sending
   either way, so this only matters as defense in depth.
3. Does the Desktop endpoint (`/exp/vDD4.40` through the engine socket) still
   work on the user's Desktop version? Detect at runtime, with a fallback to
   `localhost:12434` when TCP is enabled.
4. The runner's own `RegistryMirrors` setting: should `ddm mirror` also write
   it, so plain `docker model pull` benefits? Not needed for ddm to work.

## 9. Risks

- **The `/models/load` and tag endpoints are internal and experimental.**
  ddm should check `docker model version`, and fall back to plain
  `docker model pull` with a clear message if the load API answers 404.
  Unusable then, but never broken.
- **Hugging Face file selection must match the runner's**, or `docker model
  run hf.co/x` would not find ddm's import under the expected tag. The
  selection rules are small and tested upstream
  ([repository_gguf_test.go](https://github.com/docker/model-runner/blob/main/pkg/distribution/huggingface/repository_gguf_test.go)),
  so ddm's tests should copy those cases.

## Sources

- [docker/model-runner](https://github.com/docker/model-runner): `pkg/distribution/{distribution,huggingface,tarball}`, `pkg/inference/{api.go,models/http_handler.go,models/manager.go}`, `cmd/cli/{commands/package.go,desktop/desktop.go,desktop/context.go}`
- [docker model CLI reference](https://docs.docker.com/reference/cli/docker/model)
- [docker model package](https://docs.docker.com/reference/cli/docker/model/package/)
- [docker model pull](https://docs.docker.com/reference/cli/docker/model/pull/)
- [Get started with Docker Model Runner](https://docs.docker.com/ai/model-runner/get-started/)
- [docker desktop enable model-runner](https://docs.docker.com/reference/cli/docker/desktop/enable/model-runner.md)
- [Why Docker chose OCI artifacts for AI model packaging](https://www.docker.com/blog/why-docker-chose-oci-artifacts-for-ai-model-packaging/)
- [Docker Model Runner on Hugging Face](https://www.docker.com/blog/docker-model-runner-on-hugging-face/)
