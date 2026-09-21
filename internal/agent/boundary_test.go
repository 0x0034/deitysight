package agent

import (
	"bytes"
	"context"
	"encoding/json"
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
	c2.Storage.TaskRetention = time.Second
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
	time.Sleep(1100 * time.Millisecond)
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

func TestDownloadRejectsSameLengthCorruption(t *testing.T) {
	c := testConfig(t)
	a := openTestAgent(t, c, fixtureCollector{})
	task, _, e := a.Submit(Request{RequestID: "live-corrupt"})
	if e != nil {
		t.Fatal(e)
	}
	terminal(t, a, task.TaskID)
	p := filepath.Join(c.Storage.Path, taskPath(task.TaskID, "result.tar.gz"))
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	b[len(b)/2] ^= 0xff
	if e = os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	if w := request(t, a, "GET", "/v1/tasks/"+task.TaskID+"/result", "", c.HTTP.Token); w.Code != 409 {
		t.Fatal("corrupt result served")
	}
}

func TestExpiredPinnedTaskRetainsRecoveryMetadata(t *testing.T) {
	c := testConfig(t)
	c.Storage.ResultRetention = 10 * time.Millisecond
	c.Storage.TaskRetention = time.Second
	a := openTestAgent(t, c, fixtureCollector{})
	task, _, e := a.Submit(Request{RequestID: "pinned-crash"})
	if e != nil {
		t.Fatal(e)
	}
	terminal(t, a, task.TaskID)
	f, e := a.store.Lease(taskPath(task.TaskID, "result.tar.gz"))
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	time.Sleep(1100 * time.Millisecond)
	a.cleanup()
	if _, e = os.Stat(filepath.Join(c.Storage.Path, taskPath(task.TaskID, "task.json"))); e != nil {
		t.Fatal("metadata removed before pinned data; crash would leave an unrecoverable orphan")
	}
}

func TestBackgroundMustNotExtendTaskWindow(t *testing.T) {
	c := testConfig(t)
	c.Background.Enabled = true
	c.Background.Step = 100 * time.Millisecond
	c.Sampling.RoundTimeout = 20 * time.Millisecond
	a := openTestAgent(t, c, &secondSlowCollector{})
	task, _, e := a.Submit(Request{RequestID: "background-stall"})
	if e != nil {
		t.Fatal(e)
	}
	got := terminal(t, a, task.TaskID)
	if got.MissedPoints != 1 || got.SampledPoints != 1 {
		t.Fatalf("late background caused task window extension: %+v", got)
	}
}

type secondSlowCollector struct{ calls atomic.Int32 }

func (f *secondSlowCollector) Metadata() map[string]any { return map[string]any{} }
func (f *secondSlowCollector) Collect(ctx context.Context, emit func(Record) error) error {
	if f.calls.Add(1) == 2 {
		time.Sleep(1100 * time.Millisecond)
	}
	return emit(Record{Kind: "source", Source: "/proc/stat", Content: "cpu 1", Complete: true})
}
