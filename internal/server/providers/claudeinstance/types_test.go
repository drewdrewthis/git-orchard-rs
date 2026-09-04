package claudeinstance

import (
	"os"
	"os/exec"
	"testing"
)

// Issue #826: EPERM (process exists, owned by someone else / we lack
// permission to signal it) must read as alive, not dead — only ESRCH
// (no such process) proves death.
func TestOSLivenessChecker_IsAlive(t *testing.T) {
	checker := OSLivenessChecker{}

	t.Run("own pid is alive", func(t *testing.T) {
		if !checker.IsAlive(os.Getpid()) {
			t.Error("expected own pid to be alive")
		}
	})

	t.Run("spawned-then-reaped child is dead", func(t *testing.T) {
		cmd := exec.Command("true")
		if err := cmd.Start(); err != nil {
			t.Fatalf("setup: %v", err)
		}
		pid := cmd.Process.Pid
		if err := cmd.Wait(); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if checker.IsAlive(pid) {
			t.Error("expected reaped child pid to be dead")
		}
	})

	t.Run("pid 1 (other user, e.g. launchd/init) is alive", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can signal pid 1 directly; EPERM path not exercised")
		}
		if !checker.IsAlive(1) {
			t.Error("expected pid 1 to be alive (EPERM must read as alive, not dead)")
		}
	})
}
