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
	"cpu host 1700000000 2023/11/14 22:13:20 100 100 0 1 2 3 4 5 6 7 8 9 10 11 12 13\n" +
	"CPL host 1700000000 2023/11/14 22:13:20 100 4 0 0 0 0 0\n" +
	"PSI host 1700000000 2023/11/14 22:13:20 100 n 0 0 0\n" +
	"PRG host 1700000000 2023/11/14 22:13:20 100 42 (worker) S 0 0 42 2 0 1699999900 (/bin/worker --password TOP_SECRET (nested)) 1 0 2 0 0 0 0 0 0 0 0 y 0 0 abcdef123456 N\n" +
	"PRC host 1700000000 2023/11/14 22:13:20 100 42 (worker) S 100 2 3 0 120 0 0 1 0 42 y 0 (0)\n" +
	"PRC host 1700000000 2023/11/14 22:13:20 100 43 (worker) S 100 2 3 0 120 0 0 1 0 42 n 0 (0)\nSEP\n" +
	"CPU host 1700000005 2023/11/14 22:13:25 5 100 4 10 20 0 100 0 0 0 0 0 0 0 0 0\n" +
	"cpu host 1700000005 2023/11/14 22:13:25 5 100 0 1 2 3 4 5 6 7 8 9 10 11 12 13\n" +
	"CPL host 1700000005 2023/11/14 22:13:25 5 4 0 0 0 0 0\n" +
	"PSI host 1700000005 2023/11/14 22:13:25 5 n 0 0 0\n" +
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
	input := "RESET\n" + strings.Split(atopFixture, "\n")[4] + "\n" + strings.Split(atopFixture, "\n")[5] + "\n" + strings.Repeat(line, 16000) + "SEP\n"
	n := 0
	if err := ParseAtopStream(strings.NewReader(input), WindowSpec{Scenes: []string{"io"}}, 4096, func(r Record) error {
		if r.Source == "atop/PRD" {
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
	v, _, err := a.Submit(Request{RequestID: "window", Scenes: []string{"cpu"}})
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
	v, _, err := a.Submit(Request{RequestID: "manifest", Scenes: []string{"cpu"}})
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

func TestScenarioMissingRequiredLabelCannotCommit(t *testing.T) {
	// A syntactically valid SEP must not turn a missing selected scene into success.
	end := strings.Index(atopFixture, "SEP\n") + 4
	input := atopFixture[:end]
	for _, label := range []string{"CPU", "PRG", "PRC"} {
		var lines []string
		for _, line := range strings.Split(input, "\n") {
			if !strings.HasPrefix(line, label+" ") {
				lines = append(lines, line)
			}
		}
		commits := 0
		err := ParseAtopStream(strings.NewReader(strings.Join(lines, "\n")), WindowSpec{Scenes: []string{"cpu"}}, 4096, func(r Record) error {
			if r.Kind == "frame_end" {
				commits++
			}
			return nil
		})
		if err == nil || commits != 0 {
			t.Fatalf("missing %s committed", label)
		}
	}
}

func TestScenarioRecoveryDoesNotCommitCorruptFrame(t *testing.T) {
	c := testConfig(t)
	a := openTestAgent(t, c, fixtureCollector{})
	id := uuid()
	task := Task{TaskID: id, SchemaVersion: 2}
	if err := a.store.Mkdir("tasks/" + id); err != nil {
		t.Fatal(err)
	}
	r := Record{SchemaVersion: 2, SampleID: uuid(), Kind: "source", Complete: true}
	b, _ := jsonBytes(r)
	r.Kind = "frame_end"
	end, _ := jsonBytes(r)
	data := append(append(b, []byte("broken\n")...), end...)
	if e := a.store.Append(taskPath(id, "samples.jsonl"), data, false, false); e != nil {
		t.Fatal(e)
	}
	if e := a.repair(&task, "samples.jsonl"); e != nil {
		t.Fatal(e)
	}
	if task.SampledPoints != 0 || task.SourceRecords != 0 {
		t.Fatalf("corrupt frame committed: %+v", task)
	}
}

func TestScenarioTimeoutPartialAndNextTask(t *testing.T) {
	c := testConfig(t)
	c.Atop.StartupGrace = 0
	c.Atop.FinishGrace = 0
	col := NewAtopCollector(c)
	col.available = nil
	calls := 0
	col.run = func(ctx context.Context, _ []string, emit func(Record) error, s WindowSpec) error {
		calls++
		input := atopFixture
		if calls == 1 {
			input = input[:strings.Index(input, "SEP\n")+4]
		}
		if e := ParseAtopStream(strings.NewReader(input), s, 4096, emit); e != nil {
			return e
		}
		if calls == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	a := openTestAgent(t, c, col)
	v, _, e := a.Submit(Request{RequestID: "timeout", Scenes: []string{"cpu"}})
	if e != nil {
		t.Fatal(e)
	}
	v = terminal(t, a, v.TaskID)
	if v.State != "partial" || v.SampledPoints != 1 || v.MissedPoints != 1 {
		t.Fatalf("timeout result: %+v", v)
	}
	v, _, e = a.Submit(Request{RequestID: "after-timeout", Scenes: []string{"cpu"}})
	if e != nil {
		t.Fatal(e)
	}
	if v = terminal(t, a, v.TaskID); v.State != "completed" {
		t.Fatalf("executor still blocked: %+v", v)
	}
}

func TestScenarioBackgroundPreemptionPreservesHistory(t *testing.T) {
	c := testConfig(t)
	c.Background.Enabled = true
	c.Background.Step = time.Second
	col := NewAtopCollector(c)
	col.available = nil
	started := make(chan struct{})
	cancelled := make(chan struct{})
	col.run = func(ctx context.Context, _ []string, emit func(Record) error, s WindowSpec) error {
		if e := ParseAtopStream(strings.NewReader(atopFixture), WindowSpec{Scenes: []string{"cpu"}}, 4096, emit); e != nil {
			return e
		}
		if s.Window == c.Background.Retention {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return ctx.Err()
		}
		return nil
	}
	a := openTestAgent(t, c, col)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("background not started")
	}
	v, _, e := a.Submit(Request{RequestID: "preempt", Scenes: []string{"cpu"}})
	if e != nil {
		t.Fatal(e)
	}
	v = terminal(t, a, v.TaskID)
	select {
	case <-cancelled:
	default:
		t.Fatal("background not cancelled")
	}
	if v.State != "completed" || v.HistoryRecords == 0 {
		t.Fatalf("history lost: %+v", v)
	}
	a.Close()
}

func TestScenarioCoverageIgnoresUncommittedTail(t *testing.T) {
	a := openTestAgent(t, testConfig(t), fixtureCollector{})
	id := uuid()
	if e := a.store.Mkdir("tasks/" + id); e != nil {
		t.Fatal(e)
	}
	r := Record{SchemaVersion: 2, Kind: "source", Source: "atop/CPU", SampleID: uuid(), Complete: true}
	b, _ := jsonBytes(r)
	data := append([]byte{}, b...)
	r.Kind = "frame_end"
	r.Source = "atop/SEP"
	b, _ = jsonBytes(r)
	data = append(data, b...)
	r.Kind = "source"
	r.Source = "atop/CPU"
	r.SampleID = uuid()
	b, _ = jsonBytes(r)
	data = append(data, b...)
	if e := a.store.Append(taskPath(id, "samples.jsonl"), data, false, false); e != nil {
		t.Fatal(e)
	}
	cov, e := a.coverage("tasks/"+id+"/", []string{"samples.jsonl"})
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range cov {
		if v.Source == "atop/CPU" && v.Records != 1 {
			t.Fatalf("tail reported as complete: %+v", v)
		}
	}
}

func TestScenarioHistoryExcludesLegacyEvidence(t *testing.T) {
	a := openTestAgent(t, testConfig(t), fixtureCollector{})
	r := Record{SchemaVersion: 1, Kind: "source", Source: "/proc/stat", Content: "legacy", Complete: true, StartedAt: time.Now(), FinishedAt: time.Now()}
	b, _ := jsonBytes(r)
	if e := a.store.Append("background/legacy.jsonl", b, false, false); e != nil {
		t.Fatal(e)
	}
	task := Task{SchemaVersion: 2, TaskID: uuid()}
	if e := a.store.Mkdir("tasks/" + task.TaskID); e != nil {
		t.Fatal(e)
	}
	if e := a.freezeHistory(&task); e != nil {
		t.Fatal(e)
	}
	if task.HistoryRecords != 0 {
		t.Fatal("v1 evidence copied into v2 history")
	}
}
