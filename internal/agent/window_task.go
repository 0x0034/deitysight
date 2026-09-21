package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (a *Agent) runWindow(id string, col windowCollector) {
	a.mu.Lock()
	t := a.tasks[id].clone()
	a.mu.Unlock()
	defer func() { a.store.EndCapture(); a.mu.Lock(); a.active = ""; a.roundDeadline = time.Time{}; a.mu.Unlock() }()
	if err := a.store.BeginCapture(); err != nil {
		t.addError("storage_unavailable")
		a.finish(&t)
		return
	}
	for _, name := range []string{"samples.jsonl", "errors.jsonl"} {
		if a.store.Append(taskPath(id, name), nil, false, false) != nil {
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
	started := time.Now()
	now := started.UTC()
	t.StartedAt = &now
	if a.publish(t) != nil {
		t.addError("storage_unavailable")
		a.finish(&t)
		return
	}
	spec := WindowSpec{Window: time.Duration(t.WindowSeconds) * time.Second, Step: time.Duration(t.StepSeconds) * time.Second, Scenes: t.Scenes, IncludeThreads: t.IncludeThreads}
	a.mu.Lock()
	a.roundDeadline = started.Add(spec.Window + a.cfg.Atop.StartupGrace + a.cfg.Atop.FinishGrace + 3*time.Second)
	a.mu.Unlock()
	pending := int64(0)
	frameCaps := map[string]bool{}
	err := col.CollectWindow(a.ctx, spec, func(r Record) error {
		r.OffsetNS = int64(time.Since(started))
		b, e := jsonBytes(r)
		if e != nil {
			return e
		}
		if e = a.store.Append(taskPath(id, "samples.jsonl"), b, true, false); e != nil {
			return fmt.Errorf("%w: write", errStorage)
		}
		if r.Kind == "source" {
			pending++
			if r.Supported != nil {
				frameCaps[r.Source] = *r.Supported
			}
			return nil
		}
		if r.Kind != "frame_end" {
			return nil
		}
		t.SourceRecords += pending
		pending = 0
		t.SampledPoints++
		for k, v := range frameCaps {
			t.Capabilities[k] = v
		}
		frameCaps = map[string]bool{}
		t.Host["hostname"] = r.Hostname
		at := r.StartedAt
		t.LastSampleAt = &at
		if e = a.store.Sync(taskPath(id, "samples.jsonl")); e != nil {
			return fmt.Errorf("%w: sync", errStorage)
		}
		return a.publish(t)
	})
	if a.ctx.Err() != nil {
		return
	} // recovery will mark and package the interrupted task
	if err != nil {
		_ = a.recordError(&t, scenarioError(err), time.Since(started))
	}
	t.MissedPoints = max(0, t.PlannedPoints-t.SampledPoints)
	if t.MissedPoints > 0 && err == nil {
		_ = a.recordError(&t, "sample_missed", time.Since(started))
	}
	t.Phase = "packaging"
	_ = a.publish(t)
	a.finish(&t)
}

// A background session is preempted when an on-demand task is admitted. Each
// complete frame is independently retained; incomplete tails remain unpublished.
func (a *Agent) backgroundWindow(col windowCollector) {
	if !a.cfg.Background.Enabled || col.Available() != nil {
		return
	}
	a.mu.Lock()
	if a.active != "" || a.degraded || a.ctx.Err() != nil {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.bgCancel = cancel
	a.mu.Unlock()
	defer func() { cancel(); a.mu.Lock(); a.bgCancel = nil; a.mu.Unlock() }()
	a.pruneBackground()
	if a.store.Available() != nil {
		return
	}
	var tmp string
	defer func() {
		if tmp != "" {
			_ = a.store.Remove(tmp)
		}
	}()
	spec := WindowSpec{Window: a.cfg.Background.Retention, Step: a.cfg.Background.Step, Scenes: allScenes}
	err := col.CollectWindow(ctx, spec, func(r Record) error {
		if tmp == "" {
			tmp = "background/" + r.SampleID + ".tmp"
		}
		b, e := jsonBytes(r)
		if e != nil {
			return e
		}
		if e = a.store.Append(tmp, b, false, false); e != nil {
			return e
		}
		if r.Kind == "frame_end" {
			if e = a.store.Sync(tmp); e != nil {
				return e
			}
			if e = a.store.Publish(tmp, strings.TrimSuffix(tmp, ".tmp")+".jsonl"); e != nil {
				return e
			}
			tmp = ""
			a.pruneBackground()
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		a.mu.Lock()
		a.paused = true
		a.mu.Unlock()
	}
}
