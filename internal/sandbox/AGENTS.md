# AGENTS.md — internal/sandbox

Phase 4 tool sandbox: opt-in, kernel-enforced **write confinement** for the
whole garess process.

- **Policy shape**: Landlock handles ONLY the write-side FS rights
  (WRITE_FILE, TRUNCATE, REMOVE_DIR/FILE, MAKE_DIR/REG/SOCK/FIFO/SYM — see
  `writeRights`). Reads, directory listing and execution are NOT handled, so
  they stay permitted everywhere. `MAKE_CHAR`/`MAKE_BLOCK` and `REFER` are
  deliberately excluded. This confines in-process file tools AND bash children
  with one domain, because the kernel enforces per-process.
- **Backends** (`Apply(Options)`): `none` (default, no-op), `auto` (Landlock
  when possible, else degrade to none), `landlock` (hard error `Result.Err`
  when it cannot apply so cmd/garess refuses to run unsandboxed). `auto` and
  `none` never return `Err`. Fail-closed is total: `applyLandlock` returns an
  `Err` Result both for ABI < 6 and for ABI <= 0 (kernel has no Landlock at
  all) — regression test `TestLandlockFailsClosedWithoutKernelSupport` (an
  ABI<=0 branch that returned no Err made explicit `landlock` silently run
  unsandboxed; found on the RPi). `GARESS_SANDBOX` env overrides the config
  backend in cmd/garess.
- **detectABI is a package-level var** so tests can simulate kernels without
  Landlock (`landlock_linux.go`); `Available()`/doctor use the real syscall.
- **Enforcement is process-wide and irreversible**: `landlock_restrict_self`
  with `LANDLOCK_RESTRICT_SELF_TSYNC` (flag 1<<3) applies to every thread.
  TSYNC needs Landlock ABI >= 6 (kernel >= 6.7); earlier ABIs fall back to
  none. On-device note (Phase 5, 2026-09-03): Raspbian's armv6 `rpi-v6`
  kernel ships with Landlock disabled entirely — `landlock_create_ruleset`
  returns ENOSYS on 6.18.34+rpt-rpi-v6, `/sys/kernel/security/lsm` lists only
  `capability` — so on the RPi 1 only the `none` fallback path is exercised.
  Handled rights are masked to the ABI via `supportedFSMask` (unknown bits →
  EINVAL).
- **Syscalls are raw `x/sys/unix`** (`SYS_LANDLOCK_*` + `unix.Syscall6`) with
  local mirror structs (`rulesetAttr` 8-byte — kernel zero-pads smaller sizes;
  `pathBeneathAttr`). No go-landlock dependency (it pulls libcap/psx → cgo,
  breaking the CGO_ENABLED=0 armv6 build). x/sys types exist but use
  non-idiomatic field names; local structs are clearer.
- **Writable dirs** (`DefaultWriteDirs`): workdir (covers project `.garess/`
  state), `os.TempDir()`, and `config.GlobalDir()` (global memory + logs).
  Extras come from config `[sandbox] write_dirs` (absolute; validated in
  config). Each dir is `MkdirAll`ed first (rules cover future content but the
  top dir must exist at rule time); unopenable dirs are skipped with a
  `slog.Warn`. A rule-add failure aborts (fail closed, no partial domain).
- **Where it runs**: `cmd/garess/runTUI`, AFTER every directory is created and
  providers are built, right before `tui.New`. Never inside tests in-process —
  Apply is permanent, so the integration test
  (`TestLandlockEnforcesWriteConfinement`) drives a **helper subprocess** (the
  test binary re-executed with `GARESS_SANDBOX_HELPER=1`, intercepted by
  `TestMain`) that applies the domain and probes write-allow/deny + read.
  Skips when ABI < 6. `doctor` reports via `sandbox.Available()` (ABI + ABI>=6).
- **Gotchas**: writes under `/tmp` are allowed by default, so denial probes in
  tests must target a path outside every default root (the package dir).
  Reads stay unrestricted by design — write-confining is the intended scope.
