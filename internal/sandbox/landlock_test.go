//go:build linux

package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"garess/internal/config"
)

// The Landlock integration test runs the real enforcement in a helper
// subprocess: Apply is process-wide and irreversible, so the test process
// itself must never call it. The helper is this same test binary, rerun with
// the helper env var set; TestMain intercepts it before any test runs.
const helperEnv = "GARESS_SANDBOX_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		runSandboxHelper()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runSandboxHelper applies a Landlock domain (workdir = the allow dir) and
// probes what is and is not possible, reporting JSON on stdout.
func runSandboxHelper() {
	allowDir := os.Getenv("GARESS_SANDBOX_ALLOW")
	denyDir := os.Getenv("GARESS_SANDBOX_DENY")

	res := Apply(Options{Backend: BackendLandlock, WorkDir: allowDir})
	probe := map[string]any{
		"active": res.Active,
		"abi":    res.ABI,
		"reason": res.Reason,
	}
	if res.Err != nil {
		probe["err"] = res.Err.Error()
	}
	if res.Active {
		okFile := filepath.Join(allowDir, "ok.txt")
		probe["write_allowed"] = errString(os.WriteFile(okFile, []byte("x"), 0o644))
		probe["mkdir_allowed"] = errString(os.MkdirAll(filepath.Join(allowDir, "sub"), 0o755))
		probe["remove_allowed"] = errString(os.Remove(okFile))
		probe["write_denied"] = errString(os.WriteFile(filepath.Join(denyDir, ".sandbox-deny-probe"), []byte("x"), 0o644))
		probe["mkdir_denied"] = errString(os.MkdirAll(filepath.Join(denyDir, ".sandbox-deny-dir"), 0o755))
		probe["read_anywhere"] = errString(func() error {
			_, err := os.ReadFile("/etc/hostname")
			return err
		}())
	}
	b, _ := json.Marshal(probe)
	os.Stdout.Write(b)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestLandlockEnforcesWriteConfinement is the end-to-end Phase 4 test: once
// the domain is applied, writes/creates under the allow dir succeed, writes
// outside it are denied by the kernel, and reads stay unrestricted.
func TestLandlockEnforcesWriteConfinement(t *testing.T) {
	if abi := detectABI(); abi < 6 {
		t.Skipf("Landlock ABI %d < 6: process-wide TSYNC enforcement not available in this kernel", abi)
	}

	allowDir := t.TempDir() // under /tmp, which is also a default writable dir
	denyDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// The deny target must not sit under a default writable root, or the
	// probe would not actually be testing the confinement.
	if underDefaultWritable(denyDir) {
		t.Fatalf("deny dir %s is under a default writable root; pick another", denyDir)
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		"GARESS_SANDBOX_ALLOW="+allowDir,
		"GARESS_SANDBOX_DENY="+denyDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	var probe map[string]any
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("helper output is not JSON: %v\n%s", err, out)
	}
	if probe["err"] != nil {
		t.Fatalf("sandbox apply failed: %v", probe)
	}
	if probe["active"] != true {
		t.Fatalf("sandbox not active: %v", probe)
	}
	if probe["write_allowed"] != "" {
		t.Errorf("write under allow dir failed: %v", probe["write_allowed"])
	}
	if probe["mkdir_allowed"] != "" {
		t.Errorf("mkdir under allow dir failed: %v", probe["mkdir_allowed"])
	}
	if probe["remove_allowed"] != "" {
		t.Errorf("remove under allow dir failed: %v", probe["remove_allowed"])
	}
	if probe["write_denied"] == "" {
		t.Error("write outside the allowlist was NOT denied")
	}
	if probe["mkdir_denied"] == "" {
		t.Error("mkdir outside the allowlist was NOT denied")
	}
	if probe["read_anywhere"] != "" {
		t.Errorf("read outside the allowlist failed (should be unrestricted): %v", probe["read_anywhere"])
	}
}

// underDefaultWritable reports whether dir sits under a root that the sandbox
// grants write access to by default (temp dir + global garess config dir).
func underDefaultWritable(dir string) bool {
	dir = filepath.Clean(dir)
	roots := []string{os.TempDir()}
	if g, err := config.GlobalDir(); err == nil {
		roots = append(roots, g)
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if dir == root || strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// TestLandlockFailsClosedWithoutKernelSupport pins the Phase 4 fail-closed
// contract: the explicit "landlock" backend must refuse to run when the
// kernel has no Landlock at all (detectABI == 0), while "auto" degrades to
// "none" instead of failing. Regression for the bug found on the RPi 1
// (kernel 6.18 rpi-v6 without Landlock), where explicit "landlock" silently
// ran unsandboxed because the ABI<=0 branch returned no Err.
func TestLandlockFailsClosedWithoutKernelSupport(t *testing.T) {
	orig := detectABI
	detectABI = func() int { return 0 } // simulate a kernel with no Landlock
	t.Cleanup(func() { detectABI = orig })

	// Explicit landlock: must be a hard error (fail closed), never a silent
	// downgrade to none.
	res := Apply(Options{Backend: BackendLandlock})
	if res.Err == nil {
		t.Fatal("explicit landlock backend on a kernel without Landlock: got no Err, want fail closed")
	}
	if res.Active {
		t.Fatal("explicit landlock backend reported Active without kernel support")
	}
	if res.Backend != BackendNone {
		t.Fatalf("explicit landlock backend = %q, want %q", res.Backend, BackendNone)
	}

	// Auto: degrades to none with an explanatory reason, never an error.
	res = Apply(Options{Backend: BackendAuto})
	if res.Err != nil {
		t.Fatalf("auto backend should degrade, got Err: %v", res.Err)
	}
	if res.Backend != BackendNone || res.Active {
		t.Fatalf("auto backend = %+v, want inactive none", res)
	}
	if res.Reason == "" {
		t.Fatal("auto backend degradation produced no explanatory reason")
	}
}
