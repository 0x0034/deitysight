package agent

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

type fileDigest struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type sourceCoverage struct {
	Source   string `json:"source"`
	Records  int64  `json:"records"`
	Complete int64  `json:"complete_records"`
	Missing  int64  `json:"incomplete_records"`
}

func sourcePattern(r Record) string {
	if r.Scope == "cgroup" {
		return "<cgroup>/" + path.Base(r.Source)
	}
	parts := strings.Split(r.Source, "/")
	if len(parts) > 2 && parts[1] == "proc" {
		if _, e := strconv.Atoi(parts[2]); e == nil {
			parts[2] = "<pid>"
			if len(parts) > 4 && parts[3] == "task" {
				parts[4] = "<tid>"
			}
			return strings.Join(parts, "/")
		}
	}
	return r.Source
}
func (a *Agent) coverage(base string, names []string) ([]sourceCoverage, error) {
	counts := map[string]*sourceCoverage{}
	for _, name := range names {
		if name == "errors.jsonl" {
			continue
		}
		f, e := a.store.Open(base + name)
		if e != nil {
			return nil, e
		}
		pending := map[string]*sourceCoverage{}
		pendingID := ""
		e = readRecords(f, 16<<20, func(r Record) error {
			target := counts
			if r.SchemaVersion == 2 {
				if pendingID != r.SampleID {
					pending = map[string]*sourceCoverage{}
					pendingID = r.SampleID
				}
				target = pending
			}
			key := sourcePattern(r)
			v := target[key]
			if v == nil {
				v = &sourceCoverage{Source: key}
				target[key] = v
			}
			v.Records++
			if r.Complete {
				v.Complete++
			} else {
				v.Missing++
			}
			if r.SchemaVersion == 2 && r.Kind == "frame_end" && r.Complete {
				for k, p := range pending {
					c := counts[k]
					if c == nil {
						c = &sourceCoverage{Source: k}
						counts[k] = c
					}
					c.Records += p.Records
					c.Complete += p.Complete
					c.Missing += p.Missing
				}
				pending = map[string]*sourceCoverage{}
				pendingID = ""
			}
			return nil
		})
		f.Close()
		if e != nil {
			return nil, e
		}
	}
	out := make([]sourceCoverage, 0, len(counts))
	for _, v := range counts {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

func digest(f io.Reader) (int64, string, error) {
	h := sha256.New()
	n, e := io.Copy(h, f)
	return n, hex.EncodeToString(h.Sum(nil)), e
}

type quotaWriter struct {
	s    *store
	name string
}

func (w quotaWriter) Write(b []byte) (int, error) {
	if err := w.s.Append(w.name, b, false, true); err != nil {
		return 0, err
	}
	return len(b), nil
}
func (a *Agent) archive(t *Task) error {
	base := "tasks/" + t.TaskID + "/"
	names := []string{"samples.jsonl", "errors.jsonl"}
	if t.BackgroundEnabled {
		names = append(names, "history.jsonl")
	}
	files := make([]fileDigest, 0, len(names))
	for _, name := range names {
		f, e := a.store.Open(base + name)
		if errors.Is(e, os.ErrNotExist) {
			e = a.store.Append(base+name, nil, false, false)
			if e == nil {
				f, e = a.store.Open(base + name)
			}
		}
		if e != nil {
			return e
		}
		n, sum, e := digest(f)
		f.Close()
		if e != nil {
			return e
		}
		files = append(files, fileDigest{name, n, sum})
	}
	snapshot := t.clone()
	snapshot.Result = Result{}
	sources, e := a.coverage(base, names)
	if e != nil {
		return e
	}
	manifest, e := jsonBytes(map[string]any{
		"schema_version": max(1, t.SchemaVersion), "agent_version": Version, "agent_id": a.id, "host": t.Host, "task": snapshot, "files": files,
		"sources":          sources,
		"source_semantics": semanticsFor(t),
	})
	if e != nil {
		return e
	}
	tmp := base + "result." + uuid() + ".tmp"
	if e = a.store.Append(tmp, nil, false, true); e != nil {
		return e
	}
	defer a.store.Remove(tmp)
	gz := gzip.NewWriter(quotaWriter{a.store, tmp})
	tw := tar.NewWriter(gz)
	put := func(name string, size int64, r io.Reader) error {
		if e := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: size, ModTime: *t.EndedAt}); e != nil {
			return e
		}
		_, e := io.Copy(tw, r)
		return e
	}
	if e = put("manifest.json", int64(len(manifest)), strings.NewReader(string(manifest))); e != nil {
		return e
	}
	for _, meta := range files {
		f, e := a.store.Open(base + meta.Name)
		if e != nil {
			return e
		}
		e = put(meta.Name, meta.Size, f)
		f.Close()
		if e != nil {
			return e
		}
	}
	if e = tw.Close(); e != nil {
		return e
	}
	if e = gz.Close(); e != nil {
		return e
	}
	if e = a.store.Sync(tmp); e != nil {
		return e
	}
	f, e := a.store.Open(tmp)
	if e != nil {
		return e
	}
	n, sum, e := digest(f)
	f.Close()
	if e != nil {
		return e
	}
	if e = a.store.Publish(tmp, base+"result.tar.gz"); e != nil {
		return e
	}
	t.Result = Result{Available: true, URL: "/v1/tasks/" + t.TaskID + "/result", Size: n, SHA256: sum}
	return nil
}

// Limits include JSON escaping overhead (six bytes per raw byte) and metadata.
func readRecords(r io.Reader, limit int64, fn func(Record) error) error {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 64<<10), int(6*limit+(64<<10)))
	for scan.Scan() {
		var rec Record
		if e := json.Unmarshal(scan.Bytes(), &rec); e != nil {
			return e
		}
		if e := fn(rec); e != nil {
			return e
		}
	}
	return scan.Err()
}
func validTask(t Task, id string) bool {
	if !validID(id) || t.TaskID != id || len(t.RequestID) == 0 || len(t.RequestID) > 128 || t.WindowSeconds <= 0 || t.WindowSeconds > 9223372036 || t.StepSeconds <= 0 || t.StepSeconds > t.WindowSeconds || t.ReceivedAt.IsZero() {
		return false
	}
	switch t.State {
	case "running":
		return t.EndedAt == nil
	case "completed", "partial", "failed", "interrupted":
		return t.EndedAt != nil && t.ResultExpiresAt != nil && t.TaskExpiresAt != nil
	default:
		return false
	}
}
func (a *Agent) recover() {
	ambiguous := map[string]bool{}
	entries, err := a.store.Entries("tasks")
	if err != nil {
		a.degraded = true
		return
	}
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !validID(id) {
			a.degraded = true
			continue
		}
		b, e := a.store.Read(taskPath(id, "task.json"))
		if errors.Is(e, os.ErrNotExist) {
			remaining, readErr := a.store.Entries("tasks/" + id)
			if readErr == nil && len(remaining) == 0 {
				_ = a.store.RemoveTask(id)
				continue
			}
		}
		var t Task
		if e != nil || json.Unmarshal(b, &t) != nil || !validTask(t, id) {
			a.degraded = true
			continue
		}
		a.tasks[id] = t
		if expired(t.TaskExpiresAt, time.Now()) {
			continue
		}
		if prev, ok := a.requests[t.RequestID]; ok && prev != id {
			a.degraded = true
			ambiguous[t.RequestID] = true
			delete(a.requests, t.RequestID)
		} else if !ambiguous[t.RequestID] {
			a.requests[t.RequestID] = id
		}
	}
	for id, t := range a.tasks {
		if t.State == "running" {
			t.State = "interrupted"
			a.endTimes(&t)
			t.addError("agent_interrupted")
			t.Phase = "packaging"
			// Persist detection time before rebuilding: later restarts must not extend TTL.
			if a.persist(t) != nil {
				a.degraded = true
				a.tasks[id] = t
				continue
			}
			a.repairAndFinish(&t)
		} else if t.State == "interrupted" && t.Phase == "packaging" {
			a.repairAndFinish(&t)
		} else if t.Result.Available && !expired(t.ResultExpiresAt, time.Now()) {
			f, e := a.store.Open(taskPath(id, "result.tar.gz"))
			if e == nil {
				var n int64
				var sum string
				n, sum, e = digest(f)
				f.Close()
				if n != t.Result.Size || sum != t.Result.SHA256 {
					e = errors.New("archive integrity mismatch")
				}
			}
			if e != nil {
				t.Result.Available = false
				t.addError("result_corrupt_or_missing")
				if a.persist(t) != nil {
					a.degraded = true
				}
			}
		}
		a.tasks[id] = t
	}
	a.cleanup()
}
func (a *Agent) repairAndFinish(t *Task) {
	for _, name := range []string{"samples.jsonl", "history.jsonl", "errors.jsonl"} {
		if name == "history.jsonl" && !t.BackgroundEnabled {
			continue
		}
		if err := a.repair(t, name); err != nil {
			t.addError("recovery_data_error")
			a.degraded = true
		}
	}
	now := time.Now().UTC()
	b, _ := jsonBytes(Record{SchemaVersion: max(1, t.SchemaVersion), SampleID: uuid(), Kind: "error", Source: "agent", Code: "agent_interrupted", StartedAt: now, FinishedAt: now})
	if a.store.Append(taskPath(t.TaskID, "errors.jsonl"), b, false, false) != nil {
		t.addError("storage_unavailable")
	}
	a.finish(t)
}
func (a *Agent) repair(t *Task, name string) error {
	p := taskPath(t.TaskID, name)
	f, e := a.store.Open(p)
	if errors.Is(e, os.ErrNotExist) {
		return a.store.Append(p, nil, false, false)
	}
	if e != nil {
		return e
	}
	defer f.Close()
	tmp := p + ".recovered.tmp"
	if e = a.store.Remove(tmp); e != nil {
		return e
	}
	if e = a.store.Append(tmp, nil, false, false); e != nil {
		return e
	}
	defer a.store.Remove(tmp)
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64<<10), int(6*(16<<20)+(64<<10)))
	damaged := false
	var sources, pendingSources int64
	pendingID := ""
	sampleIDs := map[string]bool{}
	for scan.Scan() {
		var r Record
		if json.Unmarshal(scan.Bytes(), &r) != nil || (r.SchemaVersion != 1 && r.SchemaVersion != 2) {
			damaged = true
			if t.SchemaVersion >= 2 {
				break
			}
			continue
		}
		if name == "samples.jsonl" {
			if r.SchemaVersion == 2 {
				if r.Kind == "source" {
					if pendingID != "" && pendingID != r.SampleID {
						damaged = true
						break
					}
					pendingID = r.SampleID
					pendingSources++
				}
				if r.Kind == "frame_end" {
					if pendingSources == 0 || pendingID != r.SampleID || !r.Complete {
						damaged = true
						break
					}
					pendingID = ""
					sources += pendingSources
					pendingSources = 0
					sampleIDs[r.SampleID] = true
				}
			} else {
				if r.Kind == "source" {
					sources++
				}
				sampleIDs[r.SampleID] = true
			}
			if t.LastSampleAt == nil || r.FinishedAt.After(*t.LastSampleAt) {
				v := r.FinishedAt
				t.LastSampleAt = &v
			}
		}
		b := append(append([]byte{}, scan.Bytes()...), '\n')
		if e = a.store.Append(tmp, b, false, false); e != nil {
			return e
		}
	}
	if scan.Err() != nil {
		damaged = true
	}
	// Scanner accepts an unterminated final line; durable JSONL requires newline.
	if info, e := f.Stat(); e == nil && info.Size() > 0 {
		var last [1]byte
		if _, e = f.ReadAt(last[:], info.Size()-1); e == nil && last[0] != '\n' {
			damaged = true
		}
	}
	if name == "samples.jsonl" {
		t.SourceRecords = sources
		t.SampledPoints = len(sampleIDs)
	}
	if !damaged {
		return nil
	}
	t.addError("recovery_jsonl_corrupt")
	if e = a.store.Sync(tmp); e != nil {
		return e
	}
	// Retain the original bytes for forensic inspection; never silently discard corruption.
	if e = a.store.Publish(p, p+".corrupt."+uuid()); e != nil {
		return e
	}
	if e = a.store.Publish(tmp, p); e != nil {
		return e
	}
	return nil
}

func semanticsFor(t *Task) map[string]string {
	if t.SchemaVersion >= 2 {
		return map[string]string{
			"frames":       "Only records whose sample_id has a subsequent frame_end (SEP) are committed frames. Ignore any incomplete tail. Baseline=true is RESET, cumulative since boot; later frames contain atop interval values. Do not difference interval values again.",
			"timestamps":   "epoch/interval_seconds are atop observed timestamps and elapsed seconds. Requested step is not actual elapsed time. FinishedAt is agent receive time, not an atomic host observation.",
			"format":       "atop 2.7.1 parseable labels. PRG command line is replaced by (). No raw binary or unredacted stderr is stored. All other retained values preserve atop units and scope.",
			"processes":    "Only is_process=y is retained unless include_threads=true. Do not add process totals to thread totals. Match PRG PID and start_time_epoch within each frame; boot_id and full cgroup paths are not collected.",
			"cpu":          "PRC CPU times use its hertz field. Agent does not calculate rates or utilization.",
			"io":           "DSK/PRD sectors are 512 bytes. DSK busy is milliseconds, PRD process I/O is not per-device and can include reaped-child accounting. Atop 2.7.1 does not supply await or average queue length.",
			"memory":       "PRM memory sizes are KiB, its pagesize is bytes. RSS is a point value; growth and faults are interval values. PSS is not requested. Shared memory makes summed RSS non-additive.",
			"capabilities": "supported=false denotes unavailable PSI or process IO/network counters, not measured zeros. Limitations do not make an otherwise complete basic capture partial.",
			"history":      "Background uses independent atop sessions. On-demand work preempts background, leaving observable gaps; sample timestamps and baseline flags identify boundaries.",
		}
	}
	return map[string]string{
		"proc_stat":    "CPU counters use clock_ticks; process stat is thread-group aggregate; task stat is per-thread. Do not add process and thread counters. [PT] fields can be zeroed by ptrace access checks.",
		"memory":       "proc status Vm* uses kB; stat RSS uses page_size. Threads share an address space: memory is not additive across threads.",
		"io":           "rchar/wchar count syscall bytes, not storage bytes. read_bytes/write_bytes are storage accounting; process io may include waited-for children and thread-group totals.",
		"diskstats":    "Sector counters use 512-byte sectors; time counters use milliseconds. Device-level accounting cannot establish per-process causality.",
		"pressure":     "PSI avg10/avg60/avg300 are percentages; total is microseconds. Fields and full-stall support vary by kernel.",
		"atop":         "Optional parseable `atop -P ALL <interval> 1` or raw `atop -w <temporary-file> <interval> 1` output. It is an auxiliary snapshot and does not replace raw proc/cgroup records. The agent executes only a fixed atop binary path and fixed arguments; stderr, exit failure and truncation are retained as errors. Raw bytes are Base64 encoded and temporary files are removed after reading.",
		"wchan":        "Zero is ambiguous: running state, hidden symbol or denied visibility; it does not prove absence of waiting.",
		"cgroup":       "Raw units are source-defined: v2 cpu.stat usec, cpu.max quota/period usec, memory bytes, io.stat bytes and operation counts; v1 cpuacct.usage ns, cpuacct.stat clock ticks. Ancestors are visible limits only. Hierarchical counters include descendants: do not add parent/child counters or cgroup/process totals.",
		"identity":     "Correlate agent_id, boot_id, PID, start_time_ticks and TID/thread_start_time_ticks. identity_unstable invalidates this sample's affected object records.",
		"completeness": "Only visible objects are enumerable. Missing, timeout, truncated and unstable sources are explicit errors. No Top N, ranking, derived load diagnosis or model call.",
	}
}
