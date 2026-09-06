# Getting started

This guide takes you from nothing to a working `garess` chat session. It
assumes you have an OpenAI-compatible endpoint to talk to — either a local
[llama.cpp](https://github.com/ggerganov/llama.cpp) server or a hosted
service such as DeepSeek or OpenRouter.

## 1. Get a binary

Pick one path: download a release (no tooling), run the container image, or
build from source.

### Option A — download a release (recommended)

Static single-file binaries are attached to every
[GitHub Release](https://github.com/alexewerlof/garess/releases). The
`releases/latest/download/<file>` URLs below always point at the newest
version. Verify integrity against the release's `checksums.txt`
(`sha256sum -c checksums.txt`).

| Platform                      | File                                                    |
| ----------------------------- | ------------------------------------------------------- |
| Linux x86-64                  | `garess-linux-amd64`                                    |
| Linux arm64                   | `garess-linux-arm64`                                    |
| Raspberry Pi 1 / Zero (armv6) | `garess-linux-armv6`                                    |
| macOS (Intel)                 | `garess-darwin-amd64`                                   |
| macOS (Apple Silicon)         | `garess-darwin-arm64`                                   |
| Windows (experimental)        | `garess-windows-amd64.exe` / `garess-windows-arm64.exe` |
| FreeBSD amd64 / arm64         | `garess-freebsd-amd64` / `garess-freebsd-arm64`         |

Example (Linux):

```sh
curl -sL -o garess https://github.com/alexewerlof/garess/releases/latest/download/garess-linux-amd64
chmod +x garess
./garess --version
```

`garess` is a terminal app: run it over SSH or in a terminal on the device.

### Option B — container image (Linux)

The image `ghcr.io/alexewerlof/garess` (linux/amd64, linux/arm64 and
linux/armv7) runs with `docker` or `podman`. Mount your config from step 2
and a working directory — the agentic tools write there, and `.garess/`
memory/session state lives in it too — and give it a TTY:

```sh
docker run -it --rm \
  -v ~/.config/garess:/home/garess/.config/garess \
  -v "$PWD:/work" -w /work \
  ghcr.io/alexewerlof/garess
# podman: podman run -it --rm ...   (same flags)
```

If your host uid is not 1000, add `--user $(id -u):$(id -g)` so the tools
can write to the mounted directory. For a LAN llama.cpp server add
`--network host` (or point the config at your host's IP). The Raspberry Pi 1
is _not_ served by the image — use the `garess-linux-armv6` binary from
Option A.

### Option C — build from source

You need Go ≥ 1.26 (pure-Go dependencies, no cgo) and `make`:

```sh
git clone <this repo> && cd garess
make build            # -> dist/garess
```

On a Raspberry Pi 1, cross-compile from your machine and copy the binary
over — no Go toolchain is needed on the Pi:

```sh
make build-arm        # -> dist/garess-linux-armv6 (static, GOARM=6)
scp dist/garess-linux-armv6 user@rpi1:~/
ssh user@rpi1 '~/garess-linux-armv6'
```

## 2. Configure

Create a starter config with `garess init` — it writes the fully commented
example, which is bundled into the binary (no separate file to download). Use
`-g` for the global location (created `0600`, as it may hold API keys), or
omit it to write `./config.toml` in the current folder:

```sh
garess init -g
$EDITOR ~/.config/garess/config.toml   # set your provider/endpoint
```

At minimum, point a provider at your endpoint. Endpoints must include the
`/v1` prefix:

```toml
default_provider = "llamacpp"

[[providers]]
name = "llamacpp"
endpoint = "http://127.0.0.1:8080/v1"   # local llama.cpp
api_key = ""                            # llama.cpp needs no key
model = "qwen2.5-1.5b-instruct"
```

For hosted providers, either put the key in `api_key` or export
`GA_RESS_API_KEY` (the environment variable always wins). You can define
several providers and switch between them at runtime — see
[Configuration](configuration.md).

Config is discovered in this order (each overlays the previous, project
wins):

1. `~/.config/garess/config.toml` (or `$XDG_CONFIG_HOME/garess/config.toml`)
2. `.garess/config.toml` in the working directory
3. `config.toml` in the working directory (fallback)

`garess --config <path>` overrides the search entirely.

## 3. Verify

```sh
garess doctor
```

`doctor` prints your provider, pings the endpoint, lists available models,
and reports sandbox support, configured hooks, and the AGENTS.md/SYSTEM.md
files that will apply in this directory. Fix anything it flags before
continuing.

## 4. Chat

```sh
garess
```

Type a prompt and press `enter`. Keys you will need immediately:

| Key      | Effect                                       |
| -------- | -------------------------------------------- |
| `enter`  | send the prompt                              |
| `ctrl+j` | insert a newline (multi-line prompts)        |
| `esc`    | stop the current response                    |
| `ctrl+t` | show / hide the model's thinking (reasoning) |
| `ctrl+c` | quit                                         |

`/help` inside the app lists every command.

## 5. Next steps

- [Using the TUI](usage.md) — slash commands, memory notes, sessions, and how
  to steer the model with `AGENTS.md`, `SYSTEM.md`, and skills.
- [Configuration](configuration.md) — every config field and environment
  variable.
- [Safety model](safety.md) — how tool approvals, hooks, and the sandbox
  work, and what the defaults are.
