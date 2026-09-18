package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var hostSources = []string{"stat", "loadavg", "uptime", "meminfo", "vmstat", "diskstats", "pressure/cpu", "pressure/memory", "pressure/io", "self/mountinfo"}
var objectSources = []string{"stat", "status", "io", "cgroup", "wchan"}
var containerPattern = regexp.MustCompile(`(?:^|[/:-])([a-f0-9]{64})(?:\.scope)?(?:/|$)`)

type LinuxCollector struct {
	proc       *os.Root
	limit      int64
	meta       map[string]any
	atop       AtopConfig
	runAtop    AtopRunner
	runAtopRaw AtopRawRunner
}

type AtopRunner func(context.Context, []string) ([]byte, []byte, error)
type AtopRawRunner func(context.Context, []string, string) ([]byte, error)

type taskSamplingContextKey struct{}
type taskSamplingParameters struct {
	window time.Duration
	step   time.Duration
}

func withTaskSampling(ctx context.Context, window, step time.Duration) context.Context {
	return context.WithValue(ctx, taskSamplingContextKey{}, taskSamplingParameters{window: window, step: step})
}

func taskSamplingFromContext(ctx context.Context) (taskSamplingParameters, bool) {
	p, ok := ctx.Value(taskSamplingContextKey{}).(taskSamplingParameters)
	return p, ok
}

func NewLinuxCollector(procPath string, limit int64) (*LinuxCollector, error) {
	return NewLinuxCollectorWithAtop(procPath, limit, DefaultConfig().Atop)
}

func NewLinuxCollectorWithAtop(procPath string, limit int64, atop AtopConfig) (*LinuxCollector, error) {
	return newLinuxCollector(procPath, limit, atop, nil)
}

func NewLinuxCollectorWithAtopRunner(procPath string, limit int64, atop AtopConfig, runner AtopRunner) (*LinuxCollector, error) {
	return newLinuxCollector(procPath, limit, atop, runner)
}

func NewLinuxCollectorWithAtopRawRunner(procPath string, limit int64, atop AtopConfig, runner AtopRawRunner) (*LinuxCollector, error) {
	c, err := newLinuxCollector(procPath, limit, atop, nil)
	if err != nil {
		return nil, err
	}
	c.runAtopRaw = runner
	return c, nil
}

func newLinuxCollector(procPath string, limit int64, atop AtopConfig, runner AtopRunner) (*LinuxCollector, error) {
	if limit <= 0 || limit > 16<<20 {
		return nil, errors.New("invalid source byte limit")
	}
	r, e := os.OpenRoot(procPath)
	if e != nil {
		return nil, e
	}
	if atop.Enabled && !allowedAtopBinary(atop.Binary) {
		r.Close()
		return nil, errors.New("atop binary is not whitelisted")
	}
	if atop.Enabled && (atop.Interval <= 0 || atop.Interval > 60*time.Second || atop.Interval%time.Second != 0) {
		r.Close()
		return nil, errors.New("invalid atop interval")
	}
	c := &LinuxCollector{proc: r, limit: limit, atop: atop, runAtop: runner, meta: map[string]any{"architecture": runtime.GOARCH, "page_size": os.Getpagesize(), "logical_cpus": runtime.NumCPU(), "clock_ticks": nil}}
	if atop.Enabled && c.runAtop == nil {
		c.runAtop = func(ctx context.Context, args []string) ([]byte, []byte, error) {
			cmd := exec.CommandContext(ctx, atop.Binary, args...)
			cmd.Env = []string{"LC_ALL=C", "LANG=C", "TERM=dumb"}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			return stdout.Bytes(), stderr.Bytes(), err
		}
	}
	if atop.Enabled && atop.Mode == "raw" && c.runAtopRaw == nil {
		c.runAtopRaw = func(ctx context.Context, args []string, outputPath string) ([]byte, error) {
			commandArgs := append([]string{"-w", outputPath}, args[1:]...)
			cmd := exec.CommandContext(ctx, atop.Binary, commandArgs...)
			cmd.Env = []string{"LC_ALL=C", "LANG=C", "TERM=dumb"}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			return stderr.Bytes(), err
		}
	}
	for key, p := range map[string]string{"boot_id": "sys/kernel/random/boot_id", "kernel_version": "sys/kernel/osrelease", "hostname": "sys/kernel/hostname"} {
		if b, e := c.auxiliary(r, p); e == nil {
			c.meta[key] = strings.TrimSpace(string(b))
		} else {
			c.meta[key] = nil
		}
	}
	if b, e := c.auxiliary(r, "self/auxv"); e == nil && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
		for len(b) >= 16 {
			tag, val := binary.LittleEndian.Uint64(b[:8]), binary.LittleEndian.Uint64(b[8:16])
			if tag == 17 {
				c.meta["clock_ticks"] = val
				break
			}
			b = b[16:]
		}
	}
	return c, nil
}
func (c *LinuxCollector) Close() error { return c.proc.Close() }
func (c *LinuxCollector) Metadata() map[string]any {
	out := map[string]any{}
	for k, v := range c.meta {
		out[k] = v
	}
	out["atop_enabled"] = c.atop.Enabled
	if c.atop.Enabled {
		out["atop_binary"] = c.atop.Binary
		out["atop_mode"] = c.atop.Mode
		out["atop_interval_seconds"] = int64(c.atop.Interval / time.Second)
		if c.atop.Mode == "raw" {
			out["atop_path"] = c.atop.Path
		}
	}
	return out
}

func (c *LinuxCollector) collectAtop(ctx context.Context, emit func(Record) error) error {
	if !c.atop.Enabled {
		return nil
	}
	task, ok := taskSamplingFromContext(ctx)
	if !ok {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	seconds := int64(c.atop.Interval / time.Second)
	if task.step > 0 {
		seconds = int64(task.step / time.Second)
	}
	if c.atop.Mode == "raw" {
		return c.collectAtopRaw(ctx, seconds, emit)
	}
	stdout, stderr, err := c.runAtop(ctx, []string{"-P", "ALL", strconv.FormatInt(seconds, 10), "1"})
	now := time.Now().UTC()
	rec := Record{Source: "/atop/parseable", Scope: "host", StartedAt: now, FinishedAt: time.Now().UTC(), Encoding: "utf8", Complete: err == nil}
	if err != nil {
		rec.Kind, rec.Code = "error", "atop_failed"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			rec.Code = "atop_timeout"
		}
		if len(stderr) > 0 {
			stdout = append(stdout, []byte("\n[stderr]\n")...)
			stdout = append(stdout, stderr...)
		}
	} else if len(stdout) == 0 {
		rec.Kind, rec.Code = "error", "atop_empty"
		rec.Complete = false
	}
	if int64(len(stdout)) > c.limit {
		stdout = stdout[:c.limit]
		rec.Complete = false
		rec.Code = "source_truncated"
	}
	if utf8.Valid(stdout) {
		rec.Content = string(stdout)
	} else {
		rec.Encoding = "base64"
		rec.Content = base64.StdEncoding.EncodeToString(stdout)
	}
	return emit(rec)
}

func (c *LinuxCollector) collectAtopRaw(ctx context.Context, seconds int64, emit func(Record) error) error {
	dir := c.atop.Path
	if dir == "" {
		return emit(Record{Kind: "error", Source: "/atop/raw", Scope: "host", Code: "atop_storage_unavailable", Complete: false})
	}
	if err := ensureNoSymlinkPath(dir); err != nil {
		return emit(Record{Kind: "error", Source: "/atop/raw", Scope: "host", Code: "atop_storage_unavailable", Complete: false})
	}
	f, err := os.CreateTemp(dir, ".atop-*.raw")
	if err != nil {
		return emit(Record{Kind: "error", Source: "/atop/raw", Scope: "host", Code: "atop_storage_unavailable", Complete: false})
	}
	outputPath := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(outputPath)
		return emit(Record{Kind: "error", Source: "/atop/raw", Scope: "host", Code: "atop_storage_unavailable", Complete: false})
	}
	_ = os.Remove(outputPath)
	defer os.Remove(outputPath)
	args := []string{"-w", strconv.FormatInt(seconds, 10), "1"}
	stderr, runErr := c.runAtopRaw(ctx, args, outputPath)
	var data []byte
	if file, openErr := os.Open(outputPath); openErr == nil {
		data, _ = io.ReadAll(io.LimitReader(file, c.limit+1))
		_ = file.Close()
	}
	now := time.Now().UTC()
	rec := Record{Source: "/atop/raw", Scope: "host", StartedAt: now, FinishedAt: time.Now().UTC(), Encoding: "base64", Complete: runErr == nil}
	if runErr != nil {
		rec.Kind, rec.Code = "error", "atop_failed"
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
			rec.Code = "atop_timeout"
		}
		if len(data) == 0 {
			data = stderr
		}
	} else if len(data) == 0 {
		rec.Kind, rec.Code = "error", "atop_empty"
		rec.Complete = false
	}
	if int64(len(data)) > c.limit {
		data = data[:c.limit]
		rec.Complete = false
		rec.Code = "source_truncated"
	}
	rec.Content = base64.StdEncoding.EncodeToString(data)
	return emit(rec)
}

func ensureNoSymlinkPath(name string) error {
	clean := filepath.Clean(name)
	if err := os.MkdirAll(clean, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("atop path must be a real directory")
	}
	return nil
}
func sourceCode(e error) string {
	switch {
	case errors.Is(e, errTruncated):
		return "source_truncated"
	case errors.Is(e, os.ErrPermission):
		return "permission_denied"
	case errors.Is(e, os.ErrNotExist):
		return "source_unavailable"
	case errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled):
		return "sample_timeout"
	default:
		return "read_failed"
	}
}

var errTruncated = errors.New("source truncated")

// Auxiliary identity/location reads obey the same byte and symlink boundaries as emitted sources.
func (c *LinuxCollector) auxiliary(r *os.Root, name string) ([]byte, error) {
	i, e := r.Lstat(name)
	if e != nil {
		return nil, e
	}
	if !i.Mode().IsRegular() {
		return nil, errors.New("non-regular source")
	}
	f, e := r.Open(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, c.limit+1))
	if int64(len(b)) > c.limit {
		return nil, errTruncated
	}
	return b, e
}
func (c *LinuxCollector) read(r *os.Root, name, source, scope string, obj *Object, emit func(Record) error) ([]byte, error) {
	rec := Record{Kind: "source", Source: source, Scope: scope, StartedAt: time.Now().UTC(), Object: obj, Encoding: "utf8", Complete: true}
	var data []byte
	i, e := r.Lstat(name)
	if e == nil && i.Mode()&os.ModeSymlink != 0 {
		e = errors.New("symlink source rejected")
	}
	if e == nil {
		var f *os.File
		f, e = r.Open(name)
		if e == nil {
			data, e = io.ReadAll(io.LimitReader(f, c.limit+1))
			f.Close()
		}
	}
	rec.FinishedAt = time.Now().UTC()
	if e != nil {
		rec.Kind = "error"
		rec.Complete = false
		rec.Code = sourceCode(e)
	} else {
		if int64(len(data)) > c.limit {
			data = data[:c.limit]
			rec.Complete = false
			rec.Code = "source_truncated"
		}
		if utf8.Valid(data) {
			rec.Content = string(data)
		} else {
			rec.Encoding = "base64"
			rec.Content = base64.StdEncoding.EncodeToString(data)
		}
	}
	if err := emit(rec); err != nil {
		return nil, err
	}
	return data, nil
}
func startTime(b []byte) string {
	s := string(b)
	end := strings.LastIndex(s, ")")
	if end < 0 {
		return ""
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 20 {
		return ""
	}
	if _, e := strconv.ParseUint(fields[19], 10, 64); e != nil {
		return ""
	}
	return fields[19]
}

type cgMount struct {
	dir, root, kind string
	controllers     map[string]bool
}

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
func mounts(b []byte) []cgMount {
	var out []cgMount
	scan := bufio.NewScanner(strings.NewReader(string(b)))
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		parts := strings.SplitN(scan.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		before, after := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(before) < 5 || len(after) < 3 || (after[0] != "cgroup" && after[0] != "cgroup2") {
			continue
		}
		m := cgMount{dir: unescapeMount(before[4]), root: unescapeMount(before[3]), kind: after[0], controllers: map[string]bool{}}
		for _, v := range strings.Split(after[2], ",") {
			m.controllers[v] = true
		}
		if filepath.IsAbs(m.dir) && path.IsAbs(m.root) {
			out = append(out, m)
		}
	}
	return out
}

type enumerationError struct{ error }

func walkEntries(ctx context.Context, r *os.Root, name string, fn func(os.DirEntry) error) error {
	d, e := r.Open(name)
	if e != nil {
		return &enumerationError{e}
	}
	defer d.Close()
	for {
		entries, e := d.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := fn(entry); err != nil {
				return err
			}
		}
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return &enumerationError{e}
		}
	}
}
func (c *LinuxCollector) Collect(ctx context.Context, emit func(Record) error) error {
	var mountData []byte
	for _, p := range hostSources {
		if e := ctx.Err(); e != nil {
			return e
		}
		b, e := c.read(c.proc, p, "/proc/"+p, "host", nil, emit)
		if e != nil {
			return e
		}
		if p == "self/mountinfo" {
			mountData = b
		}
	}
	if err := c.collectAtop(ctx, emit); err != nil {
		return err
	}
	ms := mounts(mountData)
	seen := map[string]bool{}
	return walkEntries(ctx, c.proc, ".", func(entry os.DirEntry) error {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil || pid <= 0 || !entry.IsDir() {
			return nil
		}
		dir, e := c.proc.OpenRoot(entry.Name())
		if e != nil {
			return emit(Record{Kind: "error", Source: "/proc/" + entry.Name(), Code: "process_disappeared"})
		}
		defer dir.Close()
		initial, e := c.auxiliary(dir, "stat")
		if e != nil {
			return emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/stat", Code: sourceCode(e)})
		}
		started := startTime(initial)
		if started == "" {
			return emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/stat", Code: "invalid_identity"})
		}
		obj := &Object{PID: pid, StartTime: started}
		cg, e := c.auxiliary(dir, "cgroup")
		if e == nil {
			obj.Cgroup = strings.TrimSpace(string(cg))
			if m := containerPattern.FindSubmatch(cg); m != nil {
				obj.ContainerID = string(m[1])
			}
		}
		for _, name := range objectSources {
			if e := ctx.Err(); e != nil {
				return e
			}
			if _, e := c.read(dir, name, "/proc/"+entry.Name()+"/"+name, "process", obj, emit); e != nil {
				return e
			}
		}
		e = walkEntries(ctx, dir, "task", func(thread os.DirEntry) error {
			tid, e := strconv.Atoi(thread.Name())
			if e != nil || tid <= 0 || !thread.IsDir() {
				return nil
			}
			td, e := dir.OpenRoot("task/" + thread.Name())
			if e != nil {
				return emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/task/" + thread.Name(), Code: "thread_disappeared"})
			}
			defer td.Close()
			b, e := c.auxiliary(td, "stat")
			if e != nil {
				return emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/task/" + thread.Name() + "/stat", Code: sourceCode(e)})
			}
			ts := startTime(b)
			if ts == "" {
				return emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/task/" + thread.Name() + "/stat", Code: "invalid_identity"})
			}
			to := *obj
			to.TID = tid
			to.ThreadStartTime = ts
			tcg, _ := c.auxiliary(td, "cgroup")
			to.Cgroup = strings.TrimSpace(string(tcg))
			to.ContainerID = ""
			if m := containerPattern.FindSubmatch(tcg); m != nil {
				to.ContainerID = string(m[1])
			}
			for _, name := range objectSources {
				if e := ctx.Err(); e != nil {
					return e
				}
				if _, e = c.read(td, name, "/proc/"+entry.Name()+"/task/"+thread.Name()+"/"+name, "thread", &to, emit); e != nil {
					return e
				}
			}
			b, e = c.auxiliary(td, "stat")
			if e != nil || startTime(b) != ts {
				return emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/task/" + thread.Name(), Object: &to, Code: "identity_unstable"})
			}
			return c.collectCgroups(ctx, tcg, ms, seen, emit)
		})
		if e != nil {
			var enumeration *enumerationError
			if errors.As(e, &enumeration) {
				if e = emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/task", Code: "thread_enumeration_failed"}); e != nil {
					return e
				}
			} else {
				return e
			}
		}
		final, e := c.auxiliary(dir, "stat")
		if e != nil || startTime(final) != started {
			if e = emit(Record{Kind: "error", Source: "/proc/" + entry.Name(), Object: obj, Code: "identity_unstable"}); e != nil {
				return e
			}
		}
		return c.collectCgroups(ctx, cg, ms, seen, emit)
	})
}
func (c *LinuxCollector) collectCgroups(ctx context.Context, data []byte, ms []cgMount, seen map[string]bool, emit func(Record) error) error {
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		cg := parts[2]
		if !path.IsAbs(cg) || path.Clean(cg) != cg {
			if e := emit(Record{Kind: "error", Source: "cgroup", Code: "invalid_cgroup_path"}); e != nil {
				return e
			}
			continue
		}
		matched := false
		for _, m := range ms {
			if (m.kind == "cgroup2") != (parts[0] == "0" && parts[1] == "") {
				continue
			}
			if m.kind == "cgroup" {
				match := false
				for _, ctrl := range strings.Split(parts[1], ",") {
					if m.controllers[ctrl] {
						match = true
					}
				}
				if !match {
					continue
				}
			}
			rel := strings.TrimPrefix(cg, "/")
			if m.root != "/" {
				if cg == m.root {
					rel = "."
				} else if strings.HasPrefix(cg, m.root+"/") {
					rel = strings.TrimPrefix(cg, m.root+"/")
				} else {
					continue
				}
			}
			if rel == "" {
				rel = "."
			}
			matched = true
			root, e := os.OpenRoot(m.dir)
			if e != nil {
				if e = emit(Record{Kind: "error", Source: m.dir, Code: sourceCode(e)}); e != nil {
					return e
				}
				continue
			}
			e = func() error {
				defer root.Close()
				for {
					if e := ctx.Err(); e != nil {
						return e
					}
					key := m.dir + "/" + rel
					if !seen[key] {
						seen[key] = true
						var sources []string
						if m.kind == "cgroup2" {
							sources = []string{"cpu.stat", "cpu.max", "cpu.weight", "cpuset.cpus.effective", "memory.current", "memory.stat", "memory.max", "memory.high", "memory.events", "io.stat", "io.max", "cpu.pressure", "memory.pressure", "io.pressure", "cgroup.events"}
						} else {
							for ctrl, list := range map[string][]string{"cpuacct": {"cpuacct.usage", "cpuacct.stat"}, "cpu": {"cpu.stat", "cpu.cfs_quota_us", "cpu.cfs_period_us", "cpu.shares"}, "memory": {"memory.usage_in_bytes", "memory.limit_in_bytes", "memory.stat", "memory.failcnt"}, "blkio": {"blkio.throttle.io_service_bytes", "blkio.throttle.io_serviced", "blkio.throttle.read_bps_device", "blkio.throttle.write_bps_device"}, "cpuset": {"cpuset.cpus", "cpuset.effective_cpus"}} {
								if m.controllers[ctrl] {
									sources = append(sources, list...)
								}
							}
						}
						for _, name := range sources {
							if e := ctx.Err(); e != nil {
								return e
							}
							p := path.Join(rel, name)
							if rel == "." && m.root == "/" && m.kind == "cgroup2" && rootOnlyNotApplicable(name) {
								if _, e := root.Stat(p); errors.Is(e, os.ErrNotExist) {
									if e = emit(Record{Kind: "capability", Source: filepath.Join(m.dir, p), Scope: "cgroup", Code: "not_applicable", Complete: true}); e != nil {
										return e
									}
									continue
								}
							}
							if _, e := c.read(root, p, filepath.Join(m.dir, p), "cgroup", &Object{Cgroup: path.Join(m.root, rel)}, emit); e != nil {
								return e
							}
						}
					}
					if rel == "." {
						return nil
					}
					rel = path.Dir(rel)
				}
			}()
			if e != nil {
				return e
			}
		}
		if !matched && !seen["invisible:"+line] {
			seen["invisible:"+line] = true
			if e := emit(Record{Kind: "error", Source: "cgroup", Scope: "cgroup", Object: &Object{Cgroup: cg}, Code: "cgroup_not_visible"}); e != nil {
				return e
			}
		}
	}
	return nil
}

func rootOnlyNotApplicable(name string) bool {
	switch name {
	case "cpu.max", "cpu.weight", "memory.current", "memory.max", "memory.high", "memory.events", "io.max", "cgroup.events":
		return true
	default:
		return false
	}
}
