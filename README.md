# ddm: resumable downloads for Docker images

`ddm` pulls Docker images reliably over slow, unstable connections. It talks
to registries directly, downloads every blob over several parallel HTTP Range
connections, saves progress to disk all the time, checks every sha256, and
then loads the finished image into your local Docker under its original name.

Ctrl+C, a dropped Wi-Fi link, a crash or a reboot: run the same command again
and it continues from the exact byte where it stopped.

> **Status: Milestone 2** (download engine, `ddm pull`, import into Docker,
> mirrors, config file, proxies). `ddm doctor`, the `ddm docker …`
> pass-through and Model Runner support come in later milestones.

## Install

Linux and macOS:

```sh
curl -fsSL https://hkarimi561.github.io/ddm/install.sh | bash
```

Windows (PowerShell):

```powershell
powershell -c "irm https://hkarimi561.github.io/ddm/install.ps1 | iex"
```

The installers download the latest release for your OS and CPU, check it
against the release's `checksums.txt`, and install it to `~/.local/bin`
(`/usr/local/bin` as root) or `%LOCALAPPDATA%\Programs\ddm`. The Windows
installer also adds that folder to your user PATH. Environment variables
change the defaults: `DDM_VERSION=v0.1.0` installs a specific release,
`DDM_INSTALL_DIR` picks the folder, and `DDM_NO_MODIFY_PATH=1` (Windows)
leaves PATH alone. Prebuilt archives are also on the Releases page.

## Build from source

You need Go 1.23 or newer. Docker is only needed at runtime.

```sh
go mod tidy          # resolves dependencies and writes go.sum
go test ./...
go build -o ddm ./cmd/ddm        # ddm.exe on Windows
```

## Usage

```sh
ddm pull nginx:latest
ddm pull --platform linux/arm64 ghcr.io/org/app:1.2
ddm pull -c 8 postgres:16              # 8 connections per blob
ddm pull --no-load big/image:tag       # download only, e.g. while Docker is stopped
```

| Flag | Default | Meaning |
|---|---|---|
| `--platform` | the daemon's platform | which entry of a multi-platform image to pull |
| `-c, --connections` | 4 | parallel connections per blob |
| `-p, --parallel` | 3 | blobs downloaded at once |
| `--retries` | 0 (forever) | consecutive failed attempts before giving up |
| `--stall-timeout` | 30s | reconnect when no data arrives for this long |
| `--min-speed` | 32KiB | switch to the next mirror below this speed (0 = never) |
| `--no-load` | off | keep the image in the ddm cache only |
| `--cache-dir` | see below | where blobs and partial downloads live |
| `--config` | see below | config file to use |
| `--verbose` | off | debug logs (also switches to line-based progress) |

Credentials come from `~/.docker/config.json`, including credential helpers,
so `docker login` works as usual, for mirrors too.

## Mirrors

Mirrors are set per upstream registry and tried in order, with the upstream
itself last. ddm moves on to the next mirror when the current one keeps
failing, doesn't have a blob, or stays below `min_speed` for 30 seconds.
The download continues on the new mirror from where it stopped: blobs are
identified by their digest, so bytes fetched from different mirrors fit
together.

```sh
ddm mirror add docker.io https://docker.arvancloud.ir
ddm mirror add docker.io https://mirror.example.com --first
ddm mirror add ghcr.io https://harbor.example.com/ghcr-proxy   # proxy cache under a project
ddm mirror list
ddm mirror test docker.io            # measure latency and speed of each, suggest an order
ddm mirror test docker.io --apply    # ...and save that order
ddm mirror set-default docker.io https://mirror.example.com
ddm mirror remove docker.io https://mirror.example.com
```

To put the upstream somewhere other than last, add it explicitly, for
example `ddm mirror add docker.io https://docker.io`. ddm ships with no
mirrors configured.

## Configuration

Settings are kept in a YAML file you can edit by hand. `ddm config path`
prints where it is:

- Linux: `~/.config/ddm/config.yaml` (or `$XDG_CONFIG_HOME/ddm`)
- macOS: `~/Library/Application Support/ddm/config.yaml`
- Windows: `%AppData%\ddm\config.yaml`

`$DDM_CONFIG` or `--config` pick another file.

```yaml
connections: 8
parallel: 3
retries: 0              # 0 = retry forever
stall_timeout: 30s
min_speed: 64KiB        # 0 = never switch mirrors for speed
proxy: socks5://127.0.0.1:1080
no_proxy: localhost,.corp.example
cache_dir: D:\ddm-cache
mirrors:
  docker.io:
    - https://docker.arvancloud.ir
    - https://docker.io
  ghcr.io:
    - https://harbor.example.com/ghcr-proxy
```

```sh
ddm config list            # every setting, its value and what it does
ddm config set connections 8
ddm config set proxy socks5://127.0.0.1:1080
ddm config unset proxy
```

Flags override the file for one run. `proxy` accepts `http://`, `https://`
and `socks5://` URLs. Without it, `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`
are used. Editing through `ddm config` and `ddm mirror` keeps your comments.

### Cache location

`--cache-dir`, then `$DDM_CACHE_DIR`, then `cache_dir` from the config,
otherwise:

- Linux: `~/.cache/ddm` (or `$XDG_CACHE_HOME/ddm`)
- macOS: `~/Library/Caches/ddm`
- Windows: `%LocalAppData%\ddm`

## How it works

1. **Resolve.** The manifest is fetched and a multi-platform index is narrowed
   to one platform.
2. **Check what's there.** If Docker already has the image, ddm stops. Blobs
   already in the ddm cache are skipped, so base layers shared between images
   are downloaded once.
3. **Download.** Each blob is split into chunks of at most 32 MiB, and several
   connections work through them in parallel over HTTP/1.1. HTTP/1.1 is used
   on purpose: each connection gets its own TCP stream, which is what makes
   segmenting pay off on lossy links. Redirects to CDNs are followed. If a
   server ignores Range, ddm falls back to a single stream.
4. **Persist.** Once a second, and on exit (including Ctrl+C), ddm fsyncs the
   data file and then atomically rewrites a small state file. The state file
   holds each chunk's offset and the running sha256 of the bytes received so
   far, so it never claims more than what is safely on disk.
5. **Retry.** Failures are retried with exponential backoff and jitter, with
   no limit by default. Any attempt that made progress resets the backoff, so
   ddm picks up again within seconds when the network comes back. A
   connection that delivers nothing for `--stall-timeout` is replaced.
6. **Verify.** The whole blob must match its digest. If it doesn't, each chunk
   is compared with the hash recorded while it arrived. Chunks that changed on
   disk are downloaded again on their own. If no chunk changed, the source
   sent bad data, and the blob is downloaded again from scratch (up to 3
   times).
7. **Import.** The image is streamed into `docker load` as an archive. Docker
   registers it only once everything has been read, so a failure never leaves
   a half-imported image. See [docs/image-import.md](docs/image-import.md) for
   why this method was chosen over a temporary local registry.

## Development and releases

| Workflow | Trigger | What it does |
|---|---|---|
| [CI](.github/workflows/ci.yml) | push to `main`, pull requests | vet and test on Linux, macOS and Windows; build every platform with GoReleaser (downloadable from the run's artifacts); run both installers against those builds |
| [Release](.github/workflows/release.yml) | pushing a `v*` tag | test, then publish `ddm_<os>_<arch>` archives and `checksums.txt` to a GitHub Release |
| [Pages](.github/workflows/pages.yml) | changes to `site/` or the installers | publish the showcase page from `site/` plus `install.sh` / `install.ps1` to GitHub Pages |

To cut a release:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

One-time repository setup:

1. Commit `go.sum`: run `go mod tidy` locally first.
2. Under Settings → Pages, set the source to **GitHub Actions**.

The workflows replace the `__DDM_REPO__` placeholder in the installers and
the page with the actual `owner/repo`, so the one-liners above work for
whichever repository publishes them, forks included.

## Troubleshooting

- **"the Docker daemon is not running"**: start Docker Desktop, or run
  `sudo systemctl start docker` on Linux. Use `--no-load` to download anyway.
- **"access … was denied"**: the image name may be wrong, or the registry may
  need `docker login`.
- **Downloads keep retrying**: run with `--verbose` to see every failed
  attempt and its cause.
