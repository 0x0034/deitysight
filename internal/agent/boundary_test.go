package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureWrite(t *testing.T, root, name string, b []byte) {
	t.Helper()
	p := filepath.Join(root, name)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func processFixture(t *testing.T, root string) {
	t.Helper()
	stat := "42 (worker (name)) S " + strings.Repeat("0 ", 18) + "123 0 1 0\n"
	for _, base := range []string{"42", "42/task/42"} {
		for _, name := range objectSources {
			value := "fixture\n"
			if name == "stat" {
				value = stat
			}
			if name == "cgroup" {
				value = "0::/leaf\n"
			}
			fixtureWrite(t, root, base+"/"+name, []byte(value))
		}
	}
}

func TestCgroupHierarchyRawSources(t *testing.T) {
	for _, version := range []string{"cgroup2", "cgroup"} {
		t.Run(version, func(t *testing.T) {
			root := t.TempDir()
			cg := t.TempDir()
			processFixture(t, root)
			membership := "0::/leaf\n"
			controllers := "rw"
			if version == "cgroup" {
				membership = "2:cpu,cpuacct,memory,blkio,cpuset:/leaf\n"
				controllers = "rw,cpu,cpuacct,memory,blkio,cpuset"
			}
			for _, base := range []string{"42", "42/task/42"} {
				fixtureWrite(t, root, base+"/cgroup", []byte(membership))
			}
			fixtureWrite(t, root, "self/mountinfo", []byte(fmt.Sprintf("29 1 0:26 / %s rw - %s cgroup %s\n", cg, version, controllers)))
			for _, base := range []string{".", "leaf"} {
				for _, name := range []string{"cpu.stat", "cpu.max", "cpuacct.usage", "cpuacct.stat", "memory.stat", "memory.max", "memory.usage_in_bytes", "blkio.throttle.io_serviced", "cpuset.cpus"} {
					fixtureWrite(t, cg, filepath.Join(base, name), []byte("raw limit 123\n"))
				}
			}
			col, e := NewLinuxCollector(root, 1024)
			if e != nil {
				t.Fatal(e)
			}
			defer col.Close()
			seen := map[string]int{}
			if e = col.Collect(context.Background(), func(r Record) error {
				if r.Scope == "cgroup" {
					seen[r.Source]++
					if r.Source == filepath.Join(cg, "cpu.stat") && r.Object != nil && r.Object.Cgroup != "/" {
						t.Errorf("ancestor mislabeled: %+v", r.Object)
					}
				}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			for _, name := range []string{filepath.Join(cg, "cpu.stat"), filepath.Join(cg, "leaf/cpu.stat")} {
				if seen[name] != 1 {
					t.Fatalf("source dedup/ancestor: %s = %d", name, seen[name])
				}
			}
		})
	}
}

func TestCollectorTruncationEncodingIdentityAndMetadata(t *testing.T) {
	root := t.TempDir()
	processFixture(t, root)
	fixtureWrite(t, root, "stat", bytes.Repeat([]byte("a"), 600))
	fixtureWrite(t, root, "vmstat", []byte{0xff, 0xfe, 0x00})
	aux := make([]byte, 16)
	binary.LittleEndian.PutUint64(aux, 17)
	binary.LittleEndian.PutUint64(aux[8:], 100)
	fixtureWrite(t, root, "self/auxv", aux)
	fixtureWrite(t, root, "sys/kernel/random/boot_id", []byte("fixture-boot\n"))
	col, e := NewLinuxCollector(root, 128)
	if e != nil {
		t.Fatal(e)
	}
	defer col.Close()
	if col.Metadata()["clock_ticks"] != uint64(100) || col.Metadata()["boot_id"] != "fixture-boot" {
		t.Fatal(col.Metadata())
	}
	codes := map[string]bool{}
	encoded := false
	e = col.Collect(context.Background(), func(r Record) error {
		codes[r.Code] = true
		if r.Source == "/proc/stat" && len(r.Content) != 128 {
			t.Error("unbounded source")
		}
		if r.Source == "/proc/vmstat" {
			encoded = r.Encoding == "base64" && r.Content == "//4A"
		}
		if r.Source == "/proc/42/io" {
			fixtureWrite(t, root, "42/stat", []byte("42 (gone) S "+strings.Repeat("0 ", 18)+"999 0\n"))
		}
		return nil
	})
	if e != nil || !encoded || !codes["source_truncated"] || !codes["identity_unstable"] {
		t.Fatalf("%v %v %v", e, encoded, codes)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(col.Collect(ctx, func(Record) error { return nil }), context.Canceled) {
		t.Fatal("ignored cancellation")
	}
	if _, e = NewLinuxCollector(root, 0); e == nil {
		t.Fatal("invalid limit")
	}
}

type controlledCollector struct {
	calls   atomic.Int32
	delay   time.Duration
	host    string
	payload int
}

func (f *controlledCollector) Metadata() map[string]any { return map[string]any{"boot_id": f.host} }
func (f *controlledCollector) Collect(ctx context.Context, emit func(Record) error) error {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	} // Deliberately ignores deadline to model a stalled kernel read.
	return emit(Record{Kind: "source", Source: "/proc/stat", Content: strings.Repeat("x", max(1, f.payload)), Complete: true})
}
func TestSlowCollectorMissedPointsAndHealth(t *testing.T) {
	c := testConfig(t)
	c.Sampling.RoundTimeout = 20 * time.Millisecond
	f := &controlledCollector{delay: 1100 * time.Millisecond}
	a := openTestAgent(t, c, f)
	task, _, e := a.Submit(Request{RequestID: "slow"})
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(70 * time.Millisecond)
	if w := request(t, a, "GET", "/v1/health", "", c.HTTP.Token); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if _, _, e = a.Submit(Request{RequestID: "busy-stalled"}); e == nil {
		t.Fatal("stalled collector admitted work")
	}
	got := terminal(t, a, task.TaskID)
	if got.State != "partial" || got.MissedPoints != 1 || got.SampledPoints != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestRecoveryRetainsOriginalHostAndCorruptLines(t *testing.T) {
	c := testConfig(t)
	a := openTestAgent(t, c, &controlledCollector{host: "old-boot"})
	task, _, e := a.Submit(Request{RequestID: "recover"})
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(80 * time.Millisecond)
	a.Close()
	p := filepath.Join(c.Storage.Path, taskPath(task.TaskID, "samples.jsonl"))
	f, e := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.WriteString("broken-tail")
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	b := openTestAgent(t, c, &controlledCollector{host: "new-boot"})
	got, e := b.Get(task.TaskID)
	if e != nil || got.State != "interrupted" || !got.Result.Available {
		t.Fatalf("%+v %v", got, e)
	}
	w := request(t, b, "GET", "/v1/tasks/"+task.TaskID+"/result", "", c.HTTP.Token)
	files := archiveFiles(t, w.Body.Bytes())
	var manifest struct {
		Host map[string]any `json:"host"`
	}
	if json.Unmarshal(files["manifest.json"], &manifest) != nil || manifest.Host["boot_id"] != "old-boot" {
		t.Fatal("recovered data attributed to a new boot")
	}
	if bytes.Contains(files["samples.jsonl"], []byte("broken-tail")) {
		t.Fatal("corrupt JSONL published")
	}
	oldEnd := *got.EndedAt
	b.Close()
	d := openTestAgent(t, c, fixtureCollector{})
	again, e := d.Get(task.TaskID)
	if e != nil || !again.EndedAt.Equal(oldEnd) {
		t.Fatal("recovery extended retention")
	}
}

func TestStorageFailureAndPinnedExpiry(t *testing.T) {
	c := testConfig(t)
	c.Storage.MaxBytes = 160 << 10
	a := openTestAgent(t, c, &controlledCollector{payload: 50 << 10})
	task, _, e := a.Submit(Request{RequestID: "full"})
	if e != nil {
		t.Fatal(e)
	}
	got := terminal(t, a, task.TaskID)
	if got.State != "failed" && got.State != "partial" {
		t.Fatalf("quota ignored: %+v", got)
	}
	c2 := testConfig(t)
	c2.Storage.ResultRetention = 20 * time.Millisecond
	c2.Storage.TaskRetention = 100 * time.Millisecond
	b := openTestAgent(t, c2, fixtureCollector{})
	task, _, e = b.Submit(Request{RequestID: "lease"})
	if e != nil {
		t.Fatal(e)
	}
	terminal(t, b, task.TaskID)
	lease, e := b.store.Lease(taskPath(task.TaskID, "result.tar.gz"))
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(150 * time.Millisecond)
	b.cleanup()
	raw, e := io.ReadAll(lease)
	if e != nil || len(raw) == 0 {
		t.Fatal("active download deleted")
	}
	if _, e = b.Get(task.TaskID); e == nil {
		t.Fatal("expired task still visible")
	}
	if e = lease.Close(); e != nil {
		t.Fatal(e)
	}
	b.Close()
	d := openTestAgent(t, c2, fixtureCollector{})
	if _, _, e = d.Submit(Request{RequestID: "new"}); e != nil {
		t.Fatalf("expired download left recovery conflict: %v", e)
	}
}

func TestBackgroundContinuesThroughOnDemand(t *testing.T) {
	c := testConfig(t)
	c.Background.Enabled = true
	c.Background.Step = 30 * time.Millisecond
	f := &controlledCollector{}
	a := openTestAgent(t, c, f)
	task, _, e := a.Submit(Request{RequestID: "background"})
	if e != nil {
		t.Fatal(e)
	}
	terminal(t, a, task.TaskID)
	if f.calls.Load() < 5 {
		t.Fatalf("background stopped throughout task: %d", f.calls.Load())
	}
}

func TestTerminalArtifactCorruptionPreservesState(t *testing.T) {
	c := testConfig(t)
	a := openTestAgent(t, c, fixtureCollector{})
	task, _, e := a.Submit(Request{RequestID: "artifact"})
	if e != nil {
		t.Fatal(e)
	}
	terminal(t, a, task.TaskID)
	a.Close()
	fixtureWrite(t, c.Storage.Path, taskPath(task.TaskID, "result.tar.gz"), []byte("broken"))
	b := openTestAgent(t, c, fixtureCollector{})
	got, e := b.Get(task.TaskID)
	if e != nil || got.State != "completed" || got.Result.Available {
		t.Fatalf("%+v %v", got, e)
	}
	if w := request(t, b, "GET", "/v1/tasks/"+task.TaskID+"/result", "", c.HTTP.Token); w.Code != 409 {
		t.Fatal(w.Code)
	}
}

func TestAuxiliarySourcesCannotLeakSymlinkContent(t *testing.T) {
	root := t.TempDir()
	processFixture(t, root)
	// Root confinement alone still allows symlinks to other files inside proc root.
	fixtureWrite(t, root, "42/environ", []byte("FORBIDDEN_AUXILIARY_SECRET"))
	if e := os.Remove(filepath.Join(root, "42/cgroup")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("environ", filepath.Join(root, "42/cgroup")); e != nil {
		t.Fatal(e)
	}
	col, e := NewLinuxCollector(root, 256)
	if e != nil {
		t.Fatal(e)
	}
	defer col.Close()
	var records []Record
	if e = col.Collect(context.Background(), func(r Record) error { records = append(records, r); return nil }); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(records)
	if bytes.Contains(raw, []byte("FORBIDDEN_AUXILIARY_SECRET")) {
		t.Fatal("auxiliary identity read leaked a forbidden source")
	}
}

func TestThreadCgroupsAreCollected(t *testing.T) {
	root := t.TempDir()
	cg := t.TempDir()
	processFixture(t, root)
	fixtureWrite(t, root, "42/task/42/cgroup", []byte("0::/other\n"))
	fixtureWrite(t, root, "self/mountinfo", []byte(fmt.Sprintf("29 1 0:26 / %s rw - cgroup2 cgroup rw\n", cg)))
	fixtureWrite(t, cg, "other/cpu.stat", []byte("usage_usec 123\n"))
	col, e := NewLinuxCollector(root, 1024)
	if e != nil {
		t.Fatal(e)
	}
	defer col.Close()
	found := false
	if e = col.Collect(context.Background(), func(r Record) error {
		if r.Source == filepath.Join(cg, "other/cpu.stat") && r.Content == "usage_usec 123\n" {
			found = true
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if !found {
		t.Fatal("thread-specific cgroup omitted")
	}
}

func TestSinkPermissionErrorIsNotASourceError(t *testing.T) {
	root := t.TempDir()
	processFixture(t, root)
	col, e := NewLinuxCollector(root, 1024)
	if e != nil {
		t.Fatal(e)
	}
	defer col.Close()
	e = col.Collect(context.Background(), func(r Record) error {
		if r.Scope == "thread" {
			return os.ErrPermission
		}
		return nil
	})
	if !errors.Is(e, os.ErrPermission) {
		t.Fatal("sink failure swallowed as thread enumeration failure")
	}
}
