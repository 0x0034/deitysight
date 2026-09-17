//go:build integration && linux

// This test-only executable must run under a copy of deploy/deitysight.service.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/0x0034/deitysight/internal/sandbox"
)

func main() {
	if e := sandbox.RestrictProcessControl(); e != nil {
		panic(e)
	}
	for _, p := range []string{"/etc/deitysight/agent.yaml", "/proc/sys/kernel/hostname", "/sys/kernel/uevent_seqnum", "/tmp/deitysight-must-not-write"} {
		f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0600)
		if e == nil {
			f.Close()
			panic("write access unexpectedly allowed: " + p)
		}
		fmt.Println("write denied:", p)
	}
	p := "/var/lib/deitysight/probe.txt"
	if e := os.WriteFile(p, []byte("sandbox validation\n"), 0600); e != nil {
		panic(e)
	}
	fmt.Println("dedicated storage writable")
	if e := syscall.Kill(os.Getppid(), 0); e != syscall.EPERM {
		panic(fmt.Sprintf("kill: %v", e))
	}
	if e := syscall.Tgkill(os.Getppid(), os.Getppid(), 0); e != syscall.EPERM {
		panic(fmt.Sprintf("tgkill: %v", e))
	}
	if e := syscall.Tgkill(os.Getpid(), syscall.Gettid(), 0); e != nil {
		panic(e)
	}
	if _, _, e := syscall.Syscall6(syscall.SYS_PTRACE, 0, 0, 0, 0, 0, 0); e != syscall.EPERM {
		panic(fmt.Sprintf("ptrace: %v", e))
	}
	fmt.Println("external signals and ptrace denied; runtime self signals allowed")
	entries, e := os.ReadDir("/proc")
	if e != nil {
		panic(e)
	}
	visible := 0
	for _, entry := range entries {
		if _, e = strconv.Atoi(entry.Name()); e != nil {
			continue
		}
		b, e := os.ReadFile("/proc/" + entry.Name() + "/status")
		if e != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Uid:") && len(strings.Fields(line)) > 1 && strings.Fields(line)[1] != "0" {
				if _, e = os.ReadFile("/proc/" + entry.Name() + "/io"); e != nil {
					panic(e)
				}
				visible++
			}
		}
	}
	if visible == 0 {
		panic("no cross-user target visible")
	}
	fmt.Printf("cross-user process io readable: %d\n", visible)
}
