# Getting started

This guide takes you from nothing to a working `garess` chat session. It
assumes you have an OpenAI-compatible endpoint to talk to — either a local
[llama.cpp](https://github.com/ggerganov/llama.cpp) server or a hosted
service such as DeepSeek or OpenRouter.

## 1. Get a binary

### On a normal machine (Linux)

You need Go ≥ 1.26 (pure-Go dependencies, no cgo) and `make`:

```sh
git clone <this repo> && cd garess
make build            # -> dist/garess
```

### On a Raspberry Pi 1

Cross-compile from your machine and copy the binary over — no Go toolchain is
needed on the Pi:

```sh
make build-arm        # -> dist/garess-linux-armv6 (static, GOARM=6)
scp dist/garess-linux-armv6 user@rpi1:~/
ssh user@rpi1 '~/garess-linux-armv6'
```

`garess` is a terminal app: run it over SSH or in a terminal on the device.

## 2. Configure

Create a config from the example (the config file can hold API keys — keep it
private):

```sh
mkdir -p ~/.config/garess
cp example-config.toml ~/.config/garess/config.toml
chmod 600 ~/.config/garess/config.toml
$EDITOR ~/.config/garess/config.toml
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

| Key | Effect |
| --- | --- |
| `enter` | send the prompt |
| `ctrl+j` | insert a newline (multi-line prompts) |
| `esc` | stop the current response |
| `ctrl+t` | show / hide the model's thinking (reasoning) |
| `ctrl+c` | quit |

`/help` inside the app lists every command.

## 5. Next steps

- [Using the TUI](usage.md) — slash commands, memory notes, sessions, and how
  to steer the model with `AGENTS.md`, `SYSTEM.md`, and skills.
- [Configuration](configuration.md) — every config field and environment
  variable.
- [Safety model](safety.md) — how tool approvals, hooks, and the sandbox
  work, and what the defaults are.
