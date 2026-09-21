package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const atopFixture = "RESET\n" +
	"CPU host 1700000000 2023/11/14 22:13:20 100 100 4 1 2 3 4 5 6 7 8 9 10 11 12 13\n" +
	"PRG host 1700000000 2023/11/14 22:13:20 100 42 (worker) S 0 0 42 2 0 1699999900 (/bin/worker --password TOP_SECRET (nested)) 1 0 2 0 0 0 0 0 0 0 0 y 0 0 abcdef123456 N\n" +
	"PRC host 1700000000 2023/11/14 22:13:20 100 42 (worker) S 100 2 3 0 120 0 0 1 0 42 y 0 (0)\n" +
	"PRC host 1700000000 2023/11/14 22:13:20 100 43 (worker) S 100 2 3 0 120 0 0 1 0 42 n 0 (0)\nSEP\n" +
	"CPU host 1700000005 2023/11/14 22:13:25 5 100 4 10 20 0 100 0 0 0 0 0 0 0 0 0\n" +
	"PRG host 1700000005 2023/11/14 22:13:25 5 42 (worker) S 0 0 42 2 0 1699999900 (/bin/worker --password TOP_SECRET) 1 0 2 0 0 0 0 0 0 0 0 y 0 0 abcdef123456 N\n" +
	"PRC host 1700000005 2023/11/14 22:13:25 5 42 (worker) S 100 20 30 0 120 0 0 1 0 42 y 0 (0)\nSEP\n"

func TestScenarioStreamRedactsAndCommitsFrames(t *testing.T) {
	var got []Record
	err := ParseAtopStream(strings.NewReader(atopFixture), WindowSpec{Scenes: []string{"cpu"}}, 4096, func(r Record) error { got = append(got, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	ends := 0
	baselines := 0
	for _, r := range got {
		if strings.Contains(r.Content, "TOP_SECRET") || strings.Contains(r.Content, "--password") {
			t.Fatal("command line reached sink")
		}
		if r.Scope == "thread" {
			t.Fatal("thread retained by default")
		}
		if r.Kind == "frame_end" {
			ends++
			if r.Baseline {
				baselines++
			}
		}
	}
	if ends != 2 || baselines != 1 {
		t.Fatalf("frames=%d baseline=%d", ends, baselines)
	}
}
func TestScenarioStreamLargeFrameAndTruncation(t *testing.T) {
	line := "PRD host 1700000000 2023/11/14 22:13:20 100 42 (worker) S n y 1 2 3 4 0 42 n y\n"
	input := "RESET\n" + strings.Repeat(line, 16000) + "SEP\n"
	n := 0
	if err := ParseAtopStream(strings.NewReader(input), WindowSpec{Scenes: []string{"io"}}, 4096, func(r Record) error {
		if r.Kind == "source" {
			n++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 16000 {
		t.Fatalf("large frame truncated: %d", n)
	}
	for _, bad := range []string{strings.TrimSuffix(atopFixture, "SEP\n"), "RESET\n" + strings.Repeat("X", 5000), strings.Replace(atopFixture, "(/bin/worker --password TOP_SECRET (nested))", "(TOP_SECRET", 1)} {
		if err := ParseAtopStream(strings.NewReader(bad), WindowSpec{}, 4096, func(r Record) error {
			if strings.Contains(r.Content, "TOP_SECRET") {
				t.Fatal("malformed command leaked")
			}
			return nil
		}); err == nil {
			t.Fatal("invalid stream accepted")
		}
	}
	sentinel := errors.New("budget")
	if err := ParseAtopStream(strings.NewReader(atopFixture), WindowSpec{}, 4096, func(Record) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("sink error lost: %v", err)
	}
}
func TestScenarioAdmissionAndIdempotency(t *testing.T) {
	c := testConfig(t)
	a := openTestAgent(t, c, fixtureCollector{})
	body := `{"request_id":"scene","scenes":["io","cpu"],"include_threads":false}`
	w := request(t, a, "POST", "/v1/tasks", body, c.HTTP.Token)
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w = request(t, a, "POST", "/v1/tasks", `{"request_id":"scene","scenes":["cpu","io"]}`, c.HTTP.Token); w.Code != 200 {
		t.Fatalf("reordered scenes: %d", w.Code)
	}
	if w = request(t, a, "POST", "/v1/tasks", `{"request_id":"scene","scenes":["mem"]}`, c.HTTP.Token); w.Code != 409 {
		t.Fatalf("scene conflict: %d", w.Code)
	}
	if w = request(t, a, "POST", "/v1/tasks", `{"request_id":"scene","include_threads":true}`, c.HTTP.Token); w.Code != 409 {
		t.Fatalf("thread conflict: %d", w.Code)
	}
	for _, sc := range []string{`[]`, `["shell"]`, `["cpu","cpu"]`, `null`} {
		if w = request(t, a, "POST", "/v1/tasks", `{"request_id":"bad","scenes":`+sc+`}`, c.HTTP.Token); w.Code != 400 {
			t.Fatalf("invalid scenes %s: %d", sc, w.Code)
		}
	}
}
func TestUnavailableAtopRetainsHealthAPI(t *testing.T) {
	c := testConfig(t)
	col := NewAtopCollector(c)
	col.available = errors.New("atop unavailable")
	a := openTestAgent(t, c, col)
	if w := request(t, a, "GET", "/v1/health", "", c.HTTP.Token); w.Code != 503 {
		t.Fatalf("health %d", w.Code)
	}
	if w := request(t, a, "POST", "/v1/tasks", `{"request_id":"no-atop"}`, c.HTTP.Token); w.Code != 503 {
		t.Fatalf("admission %d", w.Code)
	}
}
func TestScenarioWindowRunsOnce(t *testing.T) {
	c := testConfig(t)
	col := NewAtopCollector(c)
	col.available = nil
	calls := 0
	col.run = func(ctx context.Context, args []string, emit func(Record) error, s WindowSpec) error {
		calls++
		if s.Window != time.Second || s.Step != time.Second {
			t.Fatal("window lost")
		}
		return ParseAtopStream(strings.NewReader(atopFixture), s, 4096, emit)
	}
	a := openTestAgent(t, c, col)
	v, _, err := a.Submit(Request{RequestID: "window"})
	if err != nil {
		t.Fatal(err)
	}
	task := terminal(t, a, v.TaskID)
	if task.State != "completed" || calls != 1 || task.SampledPoints != 2 {
		t.Fatalf("calls=%d task=%+v", calls, task)
	}
}

func TestScenarioManifestAndRecovery(t *testing.T) {
	c := testConfig(t)
	col := NewAtopCollector(c)
	col.available = nil
	col.run = func(ctx context.Context, _ []string, emit func(Record) error, s WindowSpec) error {
		return ParseAtopStream(strings.NewReader(atopFixture), s, 4096, emit)
	}
	a := openTestAgent(t, c, col)
	v, _, err := a.Submit(Request{RequestID: "manifest"})
	if err != nil {
		t.Fatal(err)
	}
	v = terminal(t, a, v.TaskID)
	w := request(t, a, "GET", v.Result.URL, "", c.HTTP.Token)
	files := archiveFiles(t, w.Body.Bytes())
	var manifest map[string]any
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["schema_version"] != float64(2) {
		t.Fatal("manifest still advertises legacy schema")
	}
	if !strings.Contains(string(files["manifest.json"]), "frame_end") {
		t.Fatal("frame commit semantics missing")
	}
	if strings.Contains(string(files["manifest.json"]), "proc_stat") {
		t.Fatal("manifest falsely claims proc evidence")
	}
	a.mu.Lock()
	v.State = "running"
	v.Phase = "sampling"
	v.EndedAt = nil
	v.Result = Result{}
	a.tasks[v.TaskID] = v
	err = a.persist(v)
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	b := openTestAgent(t, c, col)
	recovered, err := b.Get(v.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != "interrupted" || recovered.SampledPoints != 2 || recovered.SourceRecords == 0 {
		t.Fatalf("v2 frames lost in recovery: %+v", recovered)
	}
}
func TestScenarioRedactionCannotBeMovedIntoName(t *testing.T) {
	attack := strings.Replace(atopFixture, "/bin/worker --password TOP_SECRET (nested)", "TOP_SECRET) S 0 0 42 2 0 1699999900 (other", 1)
	err := ParseAtopStream(strings.NewReader(attack), WindowSpec{Scenes: []string{"cpu"}}, 4096, func(r Record) error {
		if strings.Contains(r.Content, "TOP_SECRET") {
			t.Fatal("command became a process name")
		}
		return nil
	})
	_ = err // rejecting ambiguous data is also safe
}

func TestPinnedAtopVersion(t *testing.T) {
	for _, s := range []string{"Version: 2.7.1 - 2026/07/30 11:52:50", "version: 2.7.1\n"} {
		if !supportedAtopVersion(s) {
			t.Fatalf("valid pinned version rejected: %q", s)
		}
	}
	for _, s := range []string{"Version: 2.7.10", "Version: 2.8.0", "Version: 2.7.1evil", ""} {
		if supportedAtopVersion(s) {
			t.Fatal("wrong version accepted")
		}
	}
}
