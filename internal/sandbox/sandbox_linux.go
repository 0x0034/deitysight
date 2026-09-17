//go:build linux && (amd64 || arm64)

// Package sandbox restricts process control independently of systemd's filesystem policy.
package sandbox

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// RestrictProcessControl installs a seccomp filter on every existing and future
// thread. tgkill remains available only for the agent's own thread group because
// Go's asynchronous preemption and crash handling use it. No ptrace attachment,
// process memory access, signals to other processes, or dynamic probes are allowed.
func RestrictProcessControl() error {
	var arch, seccomp, tgkill uint32
	var deny []uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = 0xc000003e
		seccomp = 317
		tgkill = 234
		deny = []uint32{62, 101, 129, 200, 297, 298, 310, 311, 321, 424, 440, 448}
	case "arm64":
		arch = 0xc00000b7
		seccomp = 277
		tgkill = 131
		deny = []uint32{117, 129, 130, 138, 240, 241, 270, 271, 280, 424, 440, 448}
	default:
		return fmt.Errorf("unsupported sandbox architecture")
	}
	const load = 0x20
	const equal = 0x15
	const ret = 0x06
	const allow = 0x7fff0000
	const errno = 0x00050000
	const kill = 0x80000000
	filter := []syscall.SockFilter{{Code: load, K: 4}, {Code: equal, K: arch, Jt: 1}, {Code: ret, K: kill}, {Code: load, K: 0}}
	// x32 uses the x86_64 audit architecture but a different syscall number space.
	if runtime.GOARCH == "amd64" {
		filter = append(filter, syscall.SockFilter{Code: 0x45, K: 0x40000000, Jf: 1}, syscall.SockFilter{Code: ret, K: errno | uint32(syscall.EPERM)})
	}
	for _, nr := range deny {
		filter = append(filter, syscall.SockFilter{Code: equal, K: nr, Jf: 1}, syscall.SockFilter{Code: ret, K: errno | uint32(syscall.EPERM)})
	}
	filter = append(filter, syscall.SockFilter{Code: equal, K: tgkill, Jf: 3}, syscall.SockFilter{Code: load, K: 16}, syscall.SockFilter{Code: equal, K: uint32(os.Getpid()), Jt: 1}, syscall.SockFilter{Code: ret, K: errno | uint32(syscall.EPERM)}, syscall.SockFilter{Code: ret, K: allow})
	program := syscall.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %w", e)
	}
	result, _, e := syscall.RawSyscall(uintptr(seccomp), 1, 1, uintptr(unsafe.Pointer(&program))) // SET_MODE_FILTER, TSYNC
	runtime.KeepAlive(filter)
	if e != 0 {
		return fmt.Errorf("seccomp: %w", e)
	}
	if result != 0 {
		return fmt.Errorf("seccomp thread synchronization failed")
	}
	return nil
}
