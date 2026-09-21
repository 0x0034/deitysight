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
// process memory access or dynamic probes are allowed. SIGKILL is restricted by
// kernel credentials to the dedicated collector UID (which must host no business processes).
func RestrictProcessControl() error {
	if err := ValidateIdentity(); err != nil {
		return err
	}
	var arch, seccomp, tgkill, killNR, pidfdSignal uint32
	var deny []uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = 0xc000003e
		seccomp = 317
		tgkill = 234
		killNR = 62
		pidfdSignal = 424
		deny = []uint32{101, 129, 200, 297, 298, 310, 311, 321, 440, 448}
	case "arm64":
		arch = 0xc00000b7
		seccomp = 277
		tgkill = 131
		killNR = 129
		pidfdSignal = 424
		deny = []uint32{117, 130, 138, 240, 241, 270, 271, 280, 440, 448}
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
	// Only SIGKILL for collection cancellation and signal 0 for Go's pidfd
	// feature detection. Kernel UID/capability checks enforce the process boundary.
	for _, nr := range []uint32{killNR, pidfdSignal} {
		filter = append(filter, syscall.SockFilter{Code: equal, K: nr, Jf: 5},
			syscall.SockFilter{Code: load, K: 24},
			syscall.SockFilter{Code: equal, K: 0, Jt: 2},
			syscall.SockFilter{Code: equal, K: 9, Jt: 1},
			syscall.SockFilter{Code: ret, K: errno | uint32(syscall.EPERM)},
			syscall.SockFilter{Code: ret, K: allow})
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

// ValidateIdentity fails closed for root or privilege sets that could bypass
// the kernel's cross-UID signal checks. proc/sys reads here validate security,
// and are not a source of investigation evidence.
func ValidateIdentity() error {
	if os.Getuid() == 0 || os.Geteuid() == 0 || os.Getuid() != os.Geteuid() {
		return fmt.Errorf("run as a dedicated non-root deitysight account")
	}
	header := struct {
		Version uint32
		PID     int32
	}{Version: 0x20080522}
	data := [2]struct{ Effective, Permitted, Inheritable uint32 }{}
	_, _, e := syscall.RawSyscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data)), 0)
	if e != 0 {
		return fmt.Errorf("capget: %w", e)
	}
	const allowed uint32 = 1<<2 | 1<<19 // DAC_READ_SEARCH, SYS_PTRACE only; never CAP_KILL/SETUID
	if (data[0].Effective|data[0].Permitted|data[0].Inheritable)&^allowed != 0 || data[1].Effective|data[1].Permitted|data[1].Inheritable != 0 {
		return fmt.Errorf("only CAP_DAC_READ_SEARCH and CAP_SYS_PTRACE are permitted")
	}
	return nil
}
