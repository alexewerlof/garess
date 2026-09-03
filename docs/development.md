# Development

How to build, test, measure, and contribute to `garess`. Start with
[Architecture](architecture.md) for the shape of the code, and read the
`AGENTS.md` in the package you are editing before changing it.

## Prerequisites

- Go ≥ 1.26 (see `go.mod`). Pure-Go dependencies, `CGO_ENABLED=0` — nothing
  else to install.
- A terminal. For on-device work: an SSH-capable Raspberry Pi 1 (armv6).

## Build

| Command | Produces |
| --- | --- |
| `make build` | `dist/garess` (host) |
| `make build-arm` | `dist/garess-linux-armv6` (static, `GOOS=linux GOARCH=arm GOARM=6`) |
| `make run` | `go run ./cmd/garess` |
| `make doctor` | `go run ./cmd/garess doctor` |
| `make test` / `make vet` / `make fmt` | test suite / vet / gofmt |

`GOARM=6` is required for the Pi 1: Go 1.21+ defaults cross-builds to
`GOARM=7`, and ARMv5 support was dropped. The armv6 build must stay
`CGO_ENABLED=0` (why: the Landlock sandbox deliberately uses raw
`x/sys/unix` syscalls instead of go-landlock, whose libcap/psx dependency
needs cgo).

## Testing conventions

- Tests never hit real networks: the LLM client and the harness run against
  `httptest` fake OpenAI endpoints.
- TUI tests drive Bubble Tea deterministically
  (`tea.WithInput(blockingReader{})` + `tea.WithOutput(io.Discard)`,
  `prog.Send(tea.KeyMsg{...})`; `Run()` returns the model as a **value**).
- Sandbox tests never call `Apply` with a real Landlock backend in the test
  process (irreversible): the enforcement test re-executes the test binary as
  a helper subprocess (`GARESS_SANDBOX_HELPER=1`, intercepted in `TestMain`)
  and skips when the kernel ABI < 6.
- `go test ./...` must pass before you finish.

### Cross-compiling tests for the Pi

The full unit + integration suites run on the Pi without a Go toolchain:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go test -c -o /tmp/hooks.test ./internal/hooks
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go test -c -o /tmp/sandbox.test ./internal/sandbox
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go test -c -o /tmp/harness.test ./internal/harness
scp /tmp/{hooks,sandbox,harness}.test user@rpi1:/tmp/
ssh user@rpi1 'cd ~ && /tmp/hooks.test && /tmp/sandbox.test && /tmp/harness.test'
```

Notes learned the hard way during on-device validation:

- Verify deploys by checksum, not size or timestamp (`md5sum` both sides).
  A stale binary can be byte-identical in size.
- You cannot `scp` over a **running** binary (ETXTBSY: "dest open …
  Failure"). Deploy to `/tmp` then `mv` (rename) — a running process keeps its
  old inode.
- The sandbox integration test skips on the Pi: Raspbian's armv6 `rpi-v6`
  kernels ship with Landlock disabled (ENOSYS), so only the `none` fallback
  path is exercisable on-device. Do not assume "newer kernel ⇒ Landlock".

## Logs and profiling

- Structured logs go to `~/.config/garess/garess.log` (the TUI owns stdout).
- `GARESS_STATS=1` prints per-5s rendering/input bucket stats to stderr
  (redirect stderr to a file — the TUI is on stdout). Buckets: `flush`,
  `viewport`, `view`, `adk`, `input`; plus a `tui-stats-view` line breaking
  the view cost into header/conversation/composer/status.
- `GARESS_PPROF_ADDR=:6060` starts a `net/http/pprof` server for CPU
  profiles (`curl -o p 'http://host:6060/debug/pprof/profile?seconds=30'`).
- `GARESS_KEYPROBE=1` runs the typing-latency probe in
  `internal/tui/keylatency_test.go` (cross-compile it for the Pi to measure
  per-key latency there). `internal/tui/render_bench_test.go` has the
  glamour/streaming/viewport benches.
- Rendering is the performance-critical surface on the Pi. The rules in
  `internal/tui/AGENTS.md` (coalescing, incremental chunked rendering,
  no per-frame lipgloss on large blocks) are each grounded in measured Pi
  numbers — treat them as constraints, not suggestions.

## Project conventions

- Finish changes with `go build ./... && go test ./...` and keep `gofmt`
  clean.
- Do not commit `dist/`, `.garess/`, or a root `config.toml` (gitignored —
  they can contain keys or local state).
- When you add or change a package, keep its `AGENTS.md` in sync — agents and
  humans both navigate the repo through them. Mirror config changes in
  `example-config.toml`.
- Config errors are typed (`*config.NotFoundError`,
  `*config.InvalidConfigError`) and mapped to friendly messages in
  `cmd/garess/main.go` — preserve that pattern.
- New slash commands belong in `tui.handleCommand` and in `helpText`.
- The Charm stack is pinned to **v1** (bubbletea v1.3.10, bubbles v1.0.0,
  lipgloss v1.1.0, glamour v1.0.0). Do not "upgrade" to v2.

## Status

All five roadmap phases are implemented and validated on-device on a
Raspberry Pi 1 (kernel 6.18 armv6): the agentic tool loop, hooks, the
Landlock sandbox, and a full agentic session against a LAN llama.cpp.
On-device validation found and fixed three real bugs:

1. Explicit `landlock` silently ran unsandboxed on kernels without Landlock
   (an ABI-0 branch returned no error) — it now fails closed, with a
   regression test that simulates Landlock-less kernels.
2. Hook timeouts killed only `sh`, not its children; on dash (Raspbian's
   `/bin/sh`) a forked `sleep` held the output pipe and blocked the caller
   until it finished (a 150 ms timeout took 5 s). Hooks now run in their own
   process group and timeouts kill the whole group.
3. `after_model` fired once per streamed token (134 shell spawns for one
   turn) — it now fires once per model generation, like `before_model`.
