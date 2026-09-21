package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Agent owns admission and durable task state. Only worker invokes the collector.
type Agent struct {
	cfg            Config
	collector      Collector
	store          *store
	id             string
	mu             sync.Mutex
	tasks          map[string]Task
	requests       map[string]string
	active         string
	degraded       bool
	paused         bool
	roundDeadline  time.Time
	ctx            context.Context
	cancel         context.CancelFunc
	jobs           chan string
	wg             sync.WaitGroup
	once           sync.Once
	bgCancel       context.CancelFunc
	nextBackground time.Time // worker-owned fixed background schedule
}

func New(c Config, collector Collector) (*Agent, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if collector == nil {
		return nil, errors.New("collector is required")
	}
	s, err := openStore(c.Storage)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{cfg: c, collector: collector, store: s, ctx: ctx, cancel: cancel, tasks: map[string]Task{}, requests: map[string]string{}, jobs: make(chan string, 1)}
	var identity struct {
		AgentID string `json:"agent_id"`
	}
	b, err := s.Read("identity.json")
	if errors.Is(err, os.ErrNotExist) {
		identity.AgentID = uuid()
		b, _ = jsonBytes(identity)
		err = s.Atomic("identity.json", b)
	} else if err == nil {
		err = json.Unmarshal(b, &identity)
		if !validID(identity.AgentID) {
			err = errors.New("invalid agent identity")
		}
	}
	if err != nil {
		cancel()
		s.Close()
		return nil, err
	}
	a.id = identity.AgentID
	a.recover()
	a.wg.Add(1)
	go a.worker()
	return a, nil
}
func (a *Agent) Close() {
	a.once.Do(func() { a.mu.Lock(); a.cancel(); a.mu.Unlock(); a.wg.Wait(); a.store.Close() })
}
func taskPath(id, name string) string { return "tasks/" + id + "/" + name }
func (a *Agent) persist(t Task) error {
	b, e := jsonBytes(t)
	if e != nil {
		return e
	}
	return a.store.Atomic(taskPath(t.TaskID, "task.json"), b)
}
func (a *Agent) publish(t Task) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.persist(t)
	a.tasks[t.TaskID] = t.clone()
	if err != nil {
		a.paused = true
		log.Print("task metadata persistence failed")
	}
	return err
}
func expired(at *time.Time, now time.Time) bool { return at != nil && !now.Before(*at) }
func logical(t Task, now time.Time) Task {
	t = t.clone()
	if expired(t.ResultExpiresAt, now) {
		t.Result.Available = false
		t.Result.Expired = true
	}
	return t
}
func (a *Agent) Get(id string) (Task, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.getLocked(id)
}
func (a *Agent) getLocked(id string) (Task, error) {
	t, ok := a.tasks[id]
	if !ok || expired(t.TaskExpiresAt, time.Now()) {
		return Task{}, apiError(404, "task_not_found", "task not found")
	}
	return logical(t, time.Now()), nil
}
func (a *Agent) Submit(r Request) (Task, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	bad := func() (Task, bool, error) {
		return Task{}, false, apiError(400, "invalid_request", "invalid request parameters")
	}
	if len(r.RequestID) < 1 || len(r.RequestID) > 128 || !utf8.ValidString(r.RequestID) {
		return bad()
	}
	for _, v := range []*int64{r.WindowSeconds, r.StepSeconds} {
		if v != nil && (*v <= 0 || *v > math.MaxInt64/int64(time.Second)) {
			return bad()
		}
	}
	scenes, sceneErr := normalizeScenes(r.Scenes)
	if sceneErr != nil {
		return bad()
	}
	if id, ok := a.requests[r.RequestID]; ok {
		t := a.tasks[id]
		if !expired(t.TaskExpiresAt, time.Now()) {
			if r.WindowSeconds != nil && *r.WindowSeconds != t.WindowSeconds || r.StepSeconds != nil && *r.StepSeconds != t.StepSeconds || r.Scenes != nil && !slices.Equal(scenes, t.Scenes) || r.IncludeThreads != nil && *r.IncludeThreads != t.IncludeThreads {
				return Task{}, false, apiError(409, "request_conflict", "request_id has different parameters")
			}
			return logical(t, time.Now()), true, nil
		}
	}
	w, s := a.cfg.Sampling.DefaultWindow, a.cfg.Sampling.DefaultStep
	if r.WindowSeconds != nil {
		w = time.Duration(*r.WindowSeconds) * time.Second
	}
	if r.StepSeconds != nil {
		s = time.Duration(*r.StepSeconds) * time.Second
	}
	if a.cfg.validateSampling(w, s) != nil {
		return bad()
	}
	if col, ok := a.collector.(windowCollector); ok && col.Available() != nil {
		return Task{}, false, apiError(503, "atop_unavailable", "atop 2.7.1 is unavailable")
	}
	if a.ctx.Err() != nil || a.degraded || (!a.roundDeadline.IsZero() && time.Now().After(a.roundDeadline)) {
		return Task{}, false, apiError(503, "agent_unavailable", "agent recovery or collector unavailable")
	}
	if a.active != "" {
		return Task{}, false, apiError(409, "agent_busy", "another task is active")
	}
	if err := a.store.Available(); err != nil {
		return Task{}, false, apiError(507, "storage_unavailable", "storage unavailable")
	}
	t := Task{TaskID: uuid(), RequestID: r.RequestID, State: "running", Phase: "sampling", WindowSeconds: int64(w / time.Second), StepSeconds: int64(s / time.Second), PlannedPoints: len(Schedule(w, s)), ReceivedAt: time.Now().UTC(), BackgroundEnabled: a.cfg.Background.Enabled, Errors: []ErrorCount{}}
	t.Scenes = scenes
	if r.IncludeThreads != nil {
		t.IncludeThreads = *r.IncludeThreads
	}
	if _, ok := a.collector.(windowCollector); ok {
		t.SchemaVersion = 2
		t.Limitations = staticLimitations()
		t.Capabilities = map[string]bool{}
	}
	t.Host = a.collector.Metadata()
	if err := a.store.Mkdir("tasks/" + t.TaskID); err != nil {
		return Task{}, false, apiError(507, "storage_unavailable", "storage unavailable")
	}
	if err := a.persist(t); err != nil {
		a.degraded = true
		return Task{}, false, apiError(507, "storage_unavailable", "task persistence failed")
	}
	a.tasks[t.TaskID] = t
	a.requests[t.RequestID] = t.TaskID
	a.active = t.TaskID
	if a.bgCancel != nil {
		a.bgCancel()
	}
	a.jobs <- t.TaskID
	return t.clone(), false, nil
}

func (a *Agent) worker() {
	defer a.wg.Done()
	a.nextBackground = time.Now().Add(a.cfg.Background.Step)
	bg := time.NewTicker(a.cfg.Background.Step)
	defer bg.Stop()
	cleanup := time.NewTicker(60 * time.Second)
	defer cleanup.Stop()
	for {
		// Pending on-demand work wins over background/cleanup on coincident deadlines.
		select {
		case <-a.ctx.Done():
			return
		case id := <-a.jobs:
			a.run(id)
			continue
		default:
		}
		select {
		case <-a.ctx.Done():
			return
		case id := <-a.jobs:
			a.run(id)
		case <-bg.C:
			if !time.Now().Before(a.nextBackground) {
				a.background(false)
				a.advanceBackground()
			}
		case <-cleanup.C:
			a.cleanup()
		}
	}
}
func (a *Agent) collect(step, window time.Duration, taskScoped bool, emit func(Record) error) error {
	timeout := min(step, a.cfg.Sampling.RoundTimeout)
	base := a.ctx

	ctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	a.mu.Lock()
	a.roundDeadline = time.Now().Add(timeout)
	a.mu.Unlock()
	boot, _ := a.collector.Metadata()["boot_id"].(string)
	err := a.collector.Collect(ctx, func(r Record) error { r.BootID = boot; return emit(r) })
	if err == nil {
		err = ctx.Err()
	}
	a.mu.Lock()
	a.roundDeadline = time.Time{}
	a.mu.Unlock()
	return err
}
func (a *Agent) run(id string) {
	if col, ok := a.collector.(windowCollector); ok {
		a.runWindow(id, col)
		return
	}
	a.mu.Lock()
	t := a.tasks[id].clone()
	a.mu.Unlock()
	defer func() { a.store.EndCapture(); a.mu.Lock(); a.active = ""; a.mu.Unlock() }()
	if err := a.store.BeginCapture(); err != nil {
		t.addError("storage_unavailable")
		a.finish(&t)
		return
	}
	for _, name := range []string{"samples.jsonl", "errors.jsonl"} {
		if err := a.store.Append(taskPath(id, name), nil, false, false); err != nil {
			t.addError("storage_unavailable")
			a.finish(&t)
			return
		}
	}
	if t.BackgroundEnabled {
		if err := a.freezeHistory(&t); err != nil {
			t.addError("storage_unavailable")
			a.finish(&t)
			return
		}
	}
	start := time.Now()
	utc := start.UTC()
	t.StartedAt = &utc
	step := time.Duration(t.StepSeconds) * time.Second
	points := Schedule(time.Duration(t.WindowSeconds)*time.Second, step)
	for i, offset := range points {
		if a.ctx.Err() != nil {
			return
		}
		deadline := start.Add(offset)
		if time.Now().After(deadline) && i > 0 {
			t.MissedPoints++
			if a.recordError(&t, "sample_missed", offset) != nil {
				break
			}
			continue
		}
		ready, missed := a.waitPoint(deadline)
		if !ready {
			return
		}
		if missed {
			t.MissedPoints++
			if a.recordError(&t, "sample_missed", offset) != nil {
				break
			}
			continue
		}
		sampleID := uuid()
		bgName := ""
		if a.cfg.Background.Enabled && !time.Now().Before(a.nextBackground) {
			bgName = "background/" + sampleID + ".tmp"
			a.advanceBackground()
		}
		err := a.collect(step, time.Duration(t.WindowSeconds)*time.Second, true, func(r Record) error {
			r.SchemaVersion = 1
			r.SampleID = sampleID
			r.OffsetNS = int64(time.Since(start))
			if r.StartedAt.IsZero() {
				r.StartedAt = time.Now().UTC()
			}
			if r.FinishedAt.IsZero() {
				r.FinishedAt = time.Now().UTC()
			}
			b, e := jsonBytes(r)
			if e != nil {
				return e
			}
			if e = a.store.Append(taskPath(id, "samples.jsonl"), b, true, false); e != nil {
				return fmt.Errorf("%w: %v", errStorage, e)
			}
			if r.Kind == "source" {
				t.SourceRecords++
			}
			if bgName != "" {
				if e = a.store.Append(bgName, b, false, false); e != nil {
					return fmt.Errorf("%w: %v", errStorage, e)
				}
			}
			if !r.Complete && r.Code != "not_applicable" {
				t.addError(r.Code)
				if e = a.store.Append(taskPath(id, "errors.jsonl"), b, true, false); e != nil {
					return fmt.Errorf("%w: %v", errStorage, e)
				}
			}
			return nil
		})
		if bgName != "" {
			if err == nil && a.store.Sync(bgName) == nil {
				_ = a.store.Publish(bgName, strings.TrimSuffix(bgName, ".tmp")+".jsonl")
			}
			_ = a.store.Remove(bgName)
		}
		if a.ctx.Err() != nil {
			return
		}
		t.SampledPoints++
		end := time.Now().UTC()
		t.LastSampleAt = &end
		if err != nil {
			code := sourceCode(err)
			if errors.Is(err, errStorage) {
				code = "storage_unavailable"
			}
			if a.recordError(&t, code, offset) != nil || code == "storage_unavailable" {
				a.mu.Lock()
				a.paused = true
				a.mu.Unlock()
				break
			}
		}
		if e := a.store.Sync(taskPath(id, "samples.jsonl")); e != nil {
			t.addError("storage_unavailable")
			break
		}
		if e := a.store.Sync(taskPath(id, "errors.jsonl")); e != nil {
			t.addError("storage_unavailable")
			break
		}
		if a.publish(t) != nil {
			break
		}
	}
	if a.ctx.Err() != nil {
		return
	}
	t.Phase = "packaging"
	if a.publish(t) != nil {
		t.addError("storage_unavailable")
	}
	a.finish(&t)
}
func (a *Agent) recordError(t *Task, code string, offset time.Duration) error {
	t.addError(code)
	now := time.Now().UTC()
	b, _ := jsonBytes(Record{SchemaVersion: max(1, t.SchemaVersion), SampleID: uuid(), Kind: "error", Source: "agent", Code: code, StartedAt: now, FinishedAt: now, OffsetNS: int64(offset)})
	return a.store.Append(taskPath(t.TaskID, "errors.jsonl"), b, true, false)
}
func (a *Agent) endTimes(t *Task) {
	if t.EndedAt == nil {
		now := time.Now().UTC()
		t.EndedAt = &now
		r := now.Add(a.cfg.Storage.ResultRetention)
		m := now.Add(a.cfg.Storage.TaskRetention)
		t.ResultExpiresAt = &r
		t.TaskExpiresAt = &m
	}
}
func (a *Agent) finish(t *Task) {
	a.endTimes(t)
	t.Phase = "finished"
	if t.State != "interrupted" {
		t.State = "completed"
		if len(t.Errors) > 0 {
			t.State = "partial"
		}
		if t.SourceRecords == 0 {
			t.State = "failed"
		}
	}
	if err := a.archive(t); err != nil {
		t.Result.Available = false
		t.addError("archive_failed")
		if t.State != "interrupted" {
			t.State = "failed"
		}
		log.Print("task archive unavailable")
	}
	if err := a.publish(*t); err != nil {
		a.mu.Lock()
		v := a.tasks[t.TaskID]
		v.Result.Available = false
		v.addError("storage_unavailable")
		a.tasks[t.TaskID] = v
		a.mu.Unlock()
	}
}

func (a *Agent) advanceBackground() {
	for !a.nextBackground.After(time.Now()) {
		a.nextBackground = a.nextBackground.Add(a.cfg.Background.Step)
	}
}

func (a *Agent) waitPoint(deadline time.Time) (bool, bool) {
	for {
		next := deadline
		if a.cfg.Background.Enabled && a.nextBackground.Add(min(a.cfg.Background.Step, a.cfg.Sampling.RoundTimeout)).Before(next) {
			next = a.nextBackground
		}
		timer := time.NewTimer(max(0, time.Until(next)))
		select {
		case <-a.ctx.Done():
			timer.Stop()
			return false, false
		case <-timer.C:
		}
		// A task always wins when both schedules are due.
		if !time.Now().Before(deadline) {
			return true, false
		}
		// Do not start a background round whose budget overlaps the next task point.
		if time.Until(deadline) > min(a.cfg.Background.Step, a.cfg.Sampling.RoundTimeout) {
			a.background(true)
			if !time.Now().Before(deadline) {
				a.advanceBackground()
				return true, true
			}
		}
		a.advanceBackground()
	}
}

func (a *Agent) background(duringTask bool) {
	if col, ok := a.collector.(windowCollector); ok {
		a.backgroundWindow(col)
		return
	}
	if !a.cfg.Background.Enabled || a.ctx.Err() != nil {
		return
	}
	a.mu.Lock()
	busy := (a.active != "" && !duringTask) || a.degraded
	a.mu.Unlock()
	if busy {
		return
	}
	a.pruneBackground()
	if a.store.Available() != nil {
		return
	}
	a.mu.Lock()
	a.paused = false
	a.mu.Unlock()
	id := uuid()
	tmp := "background/" + id + ".tmp"
	if a.store.Append(tmp, nil, false, false) != nil {
		return
	}
	err := a.collect(a.cfg.Background.Step, 0, false, func(r Record) error {
		r.SchemaVersion = 1
		r.SampleID = id
		if r.StartedAt.IsZero() {
			r.StartedAt = time.Now().UTC()
		}
		if r.FinishedAt.IsZero() {
			r.FinishedAt = time.Now().UTC()
		}
		b, e := jsonBytes(r)
		if e != nil {
			return e
		}
		return a.store.Append(tmp, b, false, false)
	})
	if err != nil {
		now := time.Now().UTC()
		b, _ := jsonBytes(Record{SchemaVersion: 1, SampleID: id, Kind: "error", Source: "agent", Code: sourceCode(err), StartedAt: now, FinishedAt: now})
		if e := a.store.Append(tmp, b, false, false); e != nil {
			a.store.Remove(tmp)
			a.mu.Lock()
			a.paused = true
			a.mu.Unlock()
			return
		}
	}
	if a.store.Sync(tmp) != nil || a.store.Publish(tmp, "background/"+id+".jsonl") != nil {
		a.store.Remove(tmp)
	}
}
func (a *Agent) pruneBackground() {
	entries, err := a.store.Entries("background")
	if err != nil {
		return
	}
	cut := time.Now().Add(-a.cfg.Background.Retention)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cut) {
			a.store.Remove("background/" + e.Name())
		}
	}
}
func (a *Agent) freezeHistory(t *Task) error {
	name := taskPath(t.TaskID, "history.jsonl")
	if err := a.store.Append(name, nil, true, false); err != nil {
		return err
	}
	entries, err := a.store.Entries("background")
	if err != nil {
		return err
	}
	cut := time.Now().Add(-a.cfg.Background.Retention)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !strings.HasSuffix(e.Name(), ".jsonl") || info.ModTime().Before(cut) {
			continue
		}
		f, err := a.store.Open("background/" + e.Name())
		if err != nil {
			return err
		}
		err = readRecords(f, 16<<20, func(r Record) error {
			if t.SchemaVersion >= 2 && r.SchemaVersion != 2 {
				return nil
			}
			b, e := jsonBytes(r)
			if e != nil {
				return e
			}
			if e = a.store.Append(name, b, true, false); e != nil {
				return e
			}
			t.HistoryRecords++
			if t.HistoryStartedAt == nil || r.StartedAt.Before(*t.HistoryStartedAt) {
				v := r.StartedAt
				t.HistoryStartedAt = &v
			}
			if t.HistoryEndedAt == nil || r.FinishedAt.After(*t.HistoryEndedAt) {
				v := r.FinishedAt
				t.HistoryEndedAt = &v
			}
			if !r.Complete && r.Code != "not_applicable" {
				t.addError("history_" + r.Code)
			}
			return nil
		})
		f.Close()
		if err != nil {
			return err
		}
	}
	t.HistoryReason = "available_retained_history"
	if t.SchemaVersion >= 2 {
		t.HistoryReason = "retained_frames_with_session_gaps_background_preempted_for_task"
	}
	if t.HistoryRecords == 0 {
		t.HistoryReason = "no_history_yet"
	}
	return a.store.Sync(name)
}
func (a *Agent) cleanup() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for id, t := range a.tasks {
		if t.State == "running" {
			continue
		}
		if expired(t.TaskExpiresAt, now) {
			if a.store.RemoveTask(id) == nil {
				delete(a.tasks, id)
				if a.requests[t.RequestID] == id {
					delete(a.requests, t.RequestID)
				}
			}
			continue
		}
		if expired(t.ResultExpiresAt, now) {
			entries, e := a.store.Entries("tasks/" + id)
			if e == nil {
				for _, d := range entries {
					if d.Name() != "task.json" {
						a.store.Remove(taskPath(id, d.Name()))
					}
				}
			}
		}
	}
	a.pruneBackground()
}
