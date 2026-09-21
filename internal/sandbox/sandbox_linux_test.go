//go:build linux

package sandbox

import (
	"flag"
 "context"
 "time"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestProcessControlFilter(t *testing.T) {
	if os.Getenv("DEITYSIGHT_FILTER_TEST") == "1" {
		if e := RestrictProcessControl(); e != nil {
			t.Fatal(e)
		}
		if e := syscall.Kill(os.Getppid(), syscall.SIGKILL); e != syscall.EPERM {
			t.Fatalf("external kill allowed: %v", e)
		}
		if e := syscall.Tgkill(os.Getppid(), os.Getppid(), 0); e != syscall.EPERM {
			t.Fatalf("external tgkill allowed: %v", e)
		}
		if e := syscall.Tgkill(os.Getpid(), syscall.Gettid(), 0); e != nil {
			t.Fatalf("runtime self signal blocked: %v", e)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
        defer cancel()
        child := exec.CommandContext(ctx, "/bin/sleep", "10")
        started := time.Now()
        if err := child.Run(); err == nil || time.Since(started) > 2*time.Second { t.Fatalf("child not reaped within deadline: %v", err) }
        for i := 0; i < 100; i++ {
			done := make(chan struct{})
			go func() { close(done) }()
			<-done
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessControlFilter$")
	if f := flag.Lookup("test.gocoverdir"); f != nil && f.Value.String() != "" {
		cmd.Args = append(cmd.Args, "-test.gocoverdir="+f.Value.String())
	}
	if os.Geteuid() == 0 { cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid:65534, Gid:65534}} } else { t.Skip("root launcher required to test cross-UID boundary") }
    cmd.Env = append(os.Environ(), "DEITYSIGHT_FILTER_TEST=1")
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%v\n%s", e, b)
	}
}
