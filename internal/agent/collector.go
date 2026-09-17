package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
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
	proc  *os.Root
	limit int64
	meta  map[string]any
}

func NewLinuxCollector(procPath string, limit int64) (*LinuxCollector, error) {
	if limit <= 0 || limit > 16<<20 {
		return nil, errors.New("invalid source byte limit")
	}
	r, e := os.OpenRoot(procPath)
	if e != nil {
		return nil, e
	}
	c := &LinuxCollector{proc: r, limit: limit, meta: map[string]any{"architecture": runtime.GOARCH, "page_size": os.Getpagesize(), "logical_cpus": runtime.NumCPU(), "clock_ticks": nil}}
	for key, p := range map[string]string{"boot_id": "sys/kernel/random/boot_id", "kernel_version": "sys/kernel/osrelease", "hostname": "sys/kernel/hostname"} {
		if b, e := r.ReadFile(p); e == nil {
			c.meta[key] = strings.TrimSpace(string(b))
		} else {
			c.meta[key] = nil
		}
	}
	if b, e := r.ReadFile("self/auxv"); e == nil && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
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
	return out
}
func sourceCode(e error) string {
	switch {
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
func walkEntries(ctx context.Context, r *os.Root, name string, fn func(os.DirEntry) error) error {
	d, e := r.Open(name)
	if e != nil {
		return e
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
			return e
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
		initial, e := dir.ReadFile("stat")
		if e != nil {
			return emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/stat", Code: sourceCode(e)})
		}
		started := startTime(initial)
		if started == "" {
			return emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/stat", Code: "invalid_identity"})
		}
		obj := &Object{PID: pid, StartTime: started}
		cg, e := dir.ReadFile("cgroup")
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
			b, e := td.ReadFile("stat")
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
			for _, name := range objectSources {
				if e := ctx.Err(); e != nil {
					return e
				}
				if _, e = c.read(td, name, "/proc/"+entry.Name()+"/task/"+thread.Name()+"/"+name, "thread", &to, emit); e != nil {
					return e
				}
			}
			b, e = td.ReadFile("stat")
			if e != nil || startTime(b) != ts {
				return emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/task/" + thread.Name(), Object: &to, Code: "identity_unstable"})
			}
			return nil
		})
		if e != nil {
			if errors.Is(e, fs.ErrNotExist) || errors.Is(e, fs.ErrPermission) {
				if e = emit(Record{Kind: "error", Source: "/proc/" + entry.Name() + "/task", Code: "thread_enumeration_failed"}); e != nil {
					return e
				}
			} else {
				return e
			}
		}
		final, e := dir.ReadFile("stat")
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
							if rel == "." && m.kind == "cgroup2" {
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
	}
	return nil
}
