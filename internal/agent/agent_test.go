package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixtureCollector struct {
	fail  bool
	delay time.Duration
}

func (f fixtureCollector) Metadata() map[string]any {
	return map[string]any{"boot_id": "fixture-boot", "hostname": "fixture"}
}
func (f fixtureCollector) Collect(ctx context.Context, emit func(Record) error) error {
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.delay):
		}
	}
	r := Record{Kind: "source", Source: "/proc/stat", Scope: "host", StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), Content: "cpu 1 2 3 4\n", Encoding: "utf8", Complete: true}
	if err := emit(r); err != nil {
		return err
	}
	if f.fail {
		return emit(Record{Kind: "error", Source: "/proc/42/io", Code: "permission_denied", Complete: false})
	}
	return nil
}

func testConfig(t *testing.T) Config {
	t.Helper()
	c := DefaultConfig()
	c.HTTP.Token = "fixture-token-not-a-production-secret"
	c.Storage.Path = filepath.Join(t.TempDir(), "data")
	c.Storage.MinFreeBytes = 1
	c.Sampling.DefaultWindow = time.Second
	c.Sampling.DefaultStep = time.Second
	return c
}
func openTestAgent(t *testing.T, c Config, f Collector) *Agent {
	t.Helper()
	a, err := New(c, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}
func request(t *testing.T, a *Agent, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func terminal(t *testing.T, a *Agent, id string) Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := a.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if task.State != "running" {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not finish")
	return Task{}
}
func archiveFiles(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	g, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	tr := tar.NewReader(g)
	out := map[string][]byte{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(tr)
		if e != nil {
			t.Fatal(e)
		}
		out[h.Name] = b
	}
	return out
}

func TestScheduleAndConfig(t *testing.T) {
	for _, tc := range []struct {
		w, s time.Duration
		want []time.Duration
	}{
		{30 * time.Second, 5 * time.Second, []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second, 25 * time.Second, 30 * time.Second}},
		{12 * time.Second, 5 * time.Second, []time.Duration{0, 5 * time.Second, 10 * time.Second, 12 * time.Second}},
	} {
		got := Schedule(tc.w, tc.s)
		if len(got) != len(tc.want) {
			t.Fatalf("schedule: %v", got)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("schedule: %v", got)
			}
		}
	}
	c := testConfig(t)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.HTTP.Token = "" }, func(c *Config) { c.Storage.Path = "/" }, func(c *Config) { c.Sampling.DefaultStep = 2 * time.Second }, func(c *Config) { c.Storage.TaskRetention = time.Second }, func(c *Config) { c.Sampling.MaxPoints = 1 }} {
		bad := c
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid config accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	text := "http:\n  token: fixture-config-token\nstorage:\n  path: " + c.Storage.Path + "\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sampling.DefaultWindow != 30*time.Second {
		t.Fatal("defaults lost")
	}
	for _, suffix := range []string{"typo: true\n", "http:\n  token: duplicate\n"} {
		if err := os.WriteFile(path, []byte(text+suffix), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("invalid YAML accepted")
		}
	}
}

func TestHTTPValidationAndAuthentication(t *testing.T) {
	c := testConfig(t)
	a := openTestAgent(t, c, fixtureCollector{})
	for _, path := range []string{"/v1/tasks", "/v1/health", "/v1/tasks/unknown", "/v1/tasks/unknown/result"} {
		if w := request(t, a, "GET", path, "", ""); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
	for _, body := range []string{
		`{}`, `null`, `{"request_id":"x","window_seconds":null}`, `{"request_id":"x","step_seconds":0}`,
		`{"request_id":"x","step_seconds":1.5}`, `{"request_id":"x","request_id":"y"}`,
		`{"request_id":"x","command":"id"}`, `{"request_id":"x"} {}`, `{"request_id":"x","window_seconds":301}`,
		`{"request_id":"x","window_seconds":1,"step_seconds":2}`, `{"request_id":"x","step_seconds":9223372036854775807}`,
	} {
		w := request(t, a, "POST", "/v1/tasks", body, c.HTTP.Token)
		if w.Code != 400 {
			t.Errorf("body %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := request(t, a, "POST", "/v1/tasks", strings.Repeat(" ", 8193), c.HTTP.Token); w.Code != 413 {
		t.Fatalf("oversize: %d", w.Code)
	}
	if w := request(t, a, "GET", "/v1/health", "", c.HTTP.Token); w.Code != 200 {
		t.Fatalf("health: %d", w.Code)
	}
}

func TestTaskLifecycleArchiveAndReplay(t *testing.T) {
	c := testConfig(t)
	a := openTestAgent(t, c, fixtureCollector{})
	w := request(t, a, "POST", "/v1/tasks", `{"request_id":"first"}`, c.HTTP.Token)
	if w.Code != 202 {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	w = request(t, a, "POST", "/v1/tasks", `{"request_id":"first"}`, c.HTTP.Token)
	if w.Code != 200 {
		t.Fatalf("replay: %d", w.Code)
	}
	if w = request(t, a, "POST", "/v1/tasks", `{"request_id":"other"}`, c.HTTP.Token); w.Code != 409 {
		t.Fatalf("busy: %d", w.Code)
	}
	if w = request(t, a, "POST", "/v1/tasks", `{"request_id":"first","window_seconds":2}`, c.HTTP.Token); w.Code != 409 {
		t.Fatalf("conflict: %d", w.Code)
	}
	task := terminal(t, a, created.TaskID)
	if task.State != "completed" || task.SampledPoints != 2 {
		t.Fatalf("task: %+v", task)
	}
	w = request(t, a, "GET", "/v1/tasks/"+created.TaskID+"/result", "", c.HTTP.Token)
	if w.Code != 200 {
		t.Fatalf("download: %d %s", w.Code, w.Body.String())
	}
	files := archiveFiles(t, w.Body.Bytes())
	for _, name := range []string{"manifest.json", "samples.jsonl", "errors.jsonl"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("missing %s", name)
		}
	}
	if len(bytes.Split(bytes.TrimSpace(files["samples.jsonl"]), []byte("\n"))) != 2 {
		t.Fatal("missing samples")
	}
	if bytes.Contains(w.Body.Bytes(), []byte(c.HTTP.Token)) || bytes.Contains(files["manifest.json"], []byte(c.HTTP.Token)) {
		t.Fatal("token leaked")
	}
	if w.Header().Get("ETag") == "" {
		t.Fatal("no digest")
	}
	var manifest struct {
		Sources []struct {
			Source  string `json:"source"`
			Records int64  `json:"records"`
		} `json:"sources"`
		Semantics map[string]string `json:"source_semantics"`
	}
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	statCoverage := false
	for _, source := range manifest.Sources {
		if source.Source == "/proc/stat" && source.Records == 2 {
			statCoverage = true
		}
	}
	if !statCoverage || manifest.Semantics["diskstats"] == "" {
		t.Fatal("manifest lacks source coverage or units")
	}
	a.Close()
	c.Sampling.DefaultWindow = 2 * time.Second
	b := openTestAgent(t, c, fixtureCollector{})
	reused, yes, err := b.Submit(Request{RequestID: "first"})
	if err != nil || !yes || reused.TaskID != created.TaskID || reused.WindowSeconds != 1 {
		t.Fatalf("restart replay: %+v %v %v", reused, yes, err)
	}
}

func TestPartialAndInterruptedRecovery(t *testing.T) {
	c := testConfig(t)
	a := openTestAgent(t, c, fixtureCollector{fail: true})
	task, _, err := a.Submit(Request{RequestID: "partial"})
	if err != nil {
		t.Fatal(err)
	}
	if got := terminal(t, a, task.TaskID); got.State != "partial" {
		t.Fatalf("state: %s", got.State)
	}
	task, _, err = a.Submit(Request{RequestID: "interrupted"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	a.Close()
	b := openTestAgent(t, c, fixtureCollector{})
	got, err := b.Get(task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "interrupted" {
		t.Fatalf("recovery state: %s", got.State)
	}
	if _, yes, err := b.Submit(Request{RequestID: "interrupted"}); err != nil || !yes {
		t.Fatalf("lost idempotency: %v", err)
	}
}

func TestConcurrentAdmissionAndExclusiveStore(t *testing.T) {
	c := testConfig(t)
	a := openTestAgent(t, c, fixtureCollector{})
	if b, err := New(c, fixtureCollector{}); err == nil {
		b.Close()
		t.Fatal("second instance accepted")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := a.Submit(Request{RequestID: strings.Repeat("x", i+1)})
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("accepted %d", accepted)
	}
}

func TestExpiryStorageBudgetAndCorruption(t *testing.T) {
	c := testConfig(t)
	c.Storage.ResultRetention = 40 * time.Millisecond
	a := openTestAgent(t, c, fixtureCollector{})
	task, _, err := a.Submit(Request{RequestID: "expire"})
	if err != nil {
		t.Fatal(err)
	}
	terminal(t, a, task.TaskID)
	time.Sleep(60 * time.Millisecond)
	if w := request(t, a, "GET", "/v1/tasks/"+task.TaskID+"/result", "", c.HTTP.Token); w.Code != 410 {
		t.Fatalf("expired: %d", w.Code)
	}
	if _, yes, err := a.Submit(Request{RequestID: "expire"}); err != nil || !yes {
		t.Fatal("expiry repeated collection")
	}
	a.Close()
	path := filepath.Join(c.Storage.Path, "tasks", task.TaskID, "task.json")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	b := openTestAgent(t, c, fixtureCollector{})
	if _, _, err := b.Submit(Request{RequestID: "expire"}); err == nil {
		t.Fatal("corruption allowed duplicate")
	}
	c2 := testConfig(t)
	c2.Storage.MaxBytes = 128
	d := openTestAgent(t, c2, fixtureCollector{})
	if _, _, err := d.Submit(Request{RequestID: "budget"}); err == nil {
		t.Fatal("storage budget ignored")
	}
}

func TestBackgroundHistory(t *testing.T) {
	c := testConfig(t)
	c.Background.Enabled = true
	c.Background.Step = 20 * time.Millisecond
	a := openTestAgent(t, c, fixtureCollector{})
	time.Sleep(90 * time.Millisecond)
	task, _, err := a.Submit(Request{RequestID: "history"})
	if err != nil {
		t.Fatal(err)
	}
	terminal(t, a, task.TaskID)
	w := request(t, a, "GET", "/v1/tasks/"+task.TaskID+"/result", "", c.HTTP.Token)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	files := archiveFiles(t, w.Body.Bytes())
	if len(files["history.jsonl"]) == 0 {
		t.Fatal("no frozen history")
	}
}
