//go:build linux

package sandbox

import (
	"flag"
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
		if e := syscall.Kill(os.Getppid(), 0); e != syscall.EPERM {
			t.Fatalf("external kill allowed: %v", e)
		}
		if e := syscall.Tgkill(os.Getppid(), os.Getppid(), 0); e != syscall.EPERM {
			t.Fatalf("external tgkill allowed: %v", e)
		}
		if e := syscall.Tgkill(os.Getpid(), syscall.Gettid(), 0); e != nil {
			t.Fatalf("runtime self signal blocked: %v", e)
		}
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
	cmd.Env = append(os.Environ(), "DEITYSIGHT_FILTER_TEST=1")
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%v\n%s", e, b)
	}
}
