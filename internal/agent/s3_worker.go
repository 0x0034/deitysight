package agent

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

func (a *Agent) wakeUploads() {
	select {
	case a.uploadWake <- struct{}{}:
	default:
	}
}
func (a *Agent) prepareTransfer(t *Task) {
	s := t.Result.S3
	if s == nil {
		return
	}
	s.URL = ""
	s.URLExpiresAt = nil
	s.URLErrorCode = ""
	s.Paused = false
	if s.State == "uploaded" {
		return
	}
	if !t.Result.Available {
		s.State = "unavailable"
		s.LastErrorCode = "local_result_unavailable"
		return
	}
	s.Key = s.objectKey(a.id, t.TaskID, t.Result.SHA256)
	s.SHA256 = t.Result.SHA256
	s.Size = t.Result.Size
	s.State = "pending"
	s.LastErrorCode = ""
	s.NextAttemptAt = nil
	if s.Provider == "yos" && s.Size > yosMaxObjectSize {
		s.State = "unavailable"
		s.LastErrorCode = "yos_object_too_large"
	}
}
func (a *Agent) resultView(t Task) Task {
	t = t.clone()
	s := t.Result.S3
	if s == nil {
		return t
	}
	s.URL = ""
	s.URLExpiresAt = nil
	s.URLErrorCode = ""
	s.Paused = false
	if s.State != "uploaded" && t.State != "running" && expired(t.ResultExpiresAt, time.Now()) {
		s.State = "expired"
		s.LastErrorCode = "local_result_expired"
	}
	matches := s.S3Target == a.cfg.S3.target()
	if s.State != "uploaded" && s.State != "expired" && s.State != "unavailable" {
		if !a.cfg.S3.Enabled {
			s.Paused = true
			s.LastErrorCode = "s3_disabled"
		} else if !matches {
			s.Paused = true
			s.LastErrorCode = "s3_target_changed"
		}
	}
	if s.State == "uploaded" {
		if !matches {
			s.URLErrorCode = "s3_target_changed"
			return t
		}
		if a.remote == nil {
			s.URLErrorCode = "signing_unavailable"
			return t
		}
		start := time.Now().UTC().Truncate(time.Second)
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
		defer cancel()
		link, e := a.remote.Presign(ctx, s.Key, a.cfg.S3.PresignTTL)
		if e != nil {
			s.URLErrorCode = "signing_failed"
			return t
		}
		end := start.Add(a.cfg.S3.PresignTTL)
		s.URL = link
		s.URLExpiresAt = &end
	}
	return t
}

// storeTransfer is the only asynchronous mutation of a task. It never publishes
// uploaded in memory before the corresponding authoritative record is durable.
func (a *Agent) storeTransfer(id string, change func(*S3Result)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tasks[id]
	if !ok || t.Result.S3 == nil {
		return errors.New("task unavailable")
	}
	t = t.clone()
	change(t.Result.S3)
	if e := a.persist(t); e != nil {
		old := a.tasks[id].clone()
		old.Result.S3.LastErrorCode = "state_write_failed"
		retry := time.Now().UTC().Add(a.cfg.S3.RetryInitial)
		old.Result.S3.NextAttemptAt = &retry
		a.tasks[id] = old
		return e
	}
	a.tasks[id] = t
	return nil
}
func (a *Agent) uploadWorker() {
	defer a.wg.Done()
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		if a.ctx.Err() != nil {
			return
		}
		if task, ok := a.nextUpload(); ok {
			a.transfer(task)
			continue
		}
		select {
		case <-a.ctx.Done():
			return
		case <-a.uploadWake:
		case <-timer.C:
		}
	}
}
func (a *Agent) nextUpload() (Task, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	var selected Task
	var selectedAt time.Time
	for id, t := range a.tasks {
		s := t.Result.S3
		if s == nil || t.State == "running" || expired(t.TaskExpiresAt, now) {
			continue
		}
		if s.State != "uploaded" && expired(t.ResultExpiresAt, now) && s.State != "expired" {
			t = t.clone()
			s = t.Result.S3
			s.State = "expired"
			s.LastErrorCode = "local_result_expired"
			s.NextAttemptAt = nil
			if a.persist(t) == nil {
				a.tasks[id] = t
			} else {
				continue
			}
		}
		if !a.cfg.S3.Enabled || a.remote == nil || a.degraded || s.S3Target != a.cfg.S3.target() {
			continue
		}
		if s.State == "uploaded" || s.State == "unavailable" || s.State == "expired" {
			if s.UploadID == "" {
				continue
			}
		}
		if s.NextAttemptAt != nil && now.Before(*s.NextAttemptAt) {
			continue
		}
		at := t.ReceivedAt
		if s.NextAttemptAt != nil {
			at = *s.NextAttemptAt
		}
		if selected.TaskID == "" || at.Before(selectedAt) {
			selected = t.clone()
			selectedAt = at
		}
	}
	return selected, selected.TaskID != ""
}
func (a *Agent) retryTransfer(id string, err error) {
	_ = a.storeTransfer(id, func(s *S3Result) {
		if s.State != "expired" && s.State != "unavailable" && s.State != "uploaded" {
			s.State = "retry_wait"
		}
		s.LastErrorCode = remoteErrorCode(err)
		delay := a.cfg.S3.RetryInitial
		for n := 1; n < s.Attempts && delay < a.cfg.S3.RetryMax; n++ {
			if delay > a.cfg.S3.RetryMax/2 {
				delay = a.cfg.S3.RetryMax
				break
			}
			delay *= 2
		}
		delay = min(delay, a.cfg.S3.RetryMax)
		delay = delay/2 + time.Duration(rand.Int64N(max(1, int64(delay-delay/2))))
		next := time.Now().UTC().Add(delay)
		s.NextAttemptAt = &next
	})
}
func (a *Agent) transfer(t Task) {
	s := t.Result.S3
	// Expired multipart work is cleanup only: no archive is opened or uploaded.
	if s.State == "expired" || s.State == "unavailable" || s.State == "uploaded" {
		ctx, cancel := context.WithTimeout(a.ctx, min(a.cfg.S3.UploadTimeout, 10*time.Second))
		defer cancel()
		if e := a.remote.Abort(ctx, s.Key, s.UploadID); e != nil {
			a.retryTransfer(t.TaskID, e)
			return
		}
		_ = a.storeTransfer(t.TaskID, func(v *S3Result) { v.UploadID = ""; v.NextAttemptAt = nil })
		return
	}
	if t.ResultExpiresAt == nil || expired(t.ResultExpiresAt, time.Now()) {
		return
	}
	deadline := minTime(*t.ResultExpiresAt, time.Now().Add(a.cfg.S3.UploadTimeout))
	ctx, cancel := context.WithDeadline(a.ctx, deadline)
	defer cancel()
	if e := a.storeTransfer(t.TaskID, func(v *S3Result) { v.State = "uploading"; v.Attempts++; v.LastErrorCode = ""; v.NextAttemptAt = nil }); e != nil {
		return
	}
	f, e := a.store.Lease(taskPath(t.TaskID, "result.tar.gz"))
	if e != nil {
		_ = a.storeTransfer(t.TaskID, func(v *S3Result) { v.State = "unavailable"; v.LastErrorCode = "local_result_unavailable" })
		return
	}
	defer f.Close()
	n, sum, e := digest(&contextReader{ctx: ctx, r: f})
	if e != nil {
		a.retryTransfer(t.TaskID, e)
		return
	}
	if n != s.Size || sum != s.SHA256 {
		_ = a.storeTransfer(t.TaskID, func(v *S3Result) { v.State = "unavailable"; v.LastErrorCode = "local_integrity_mismatch" })
		return
	}
	if _, e = f.Seek(0, 0); e != nil {
		a.retryTransfer(t.TaskID, e)
		return
	}
	o := uploadObject{Key: s.Key, AgentID: a.id, TaskID: t.TaskID, SHA256: s.SHA256, Size: s.Size, UploadID: s.UploadID}
	e = a.remote.Check(ctx, o)
	if e != nil && !errors.Is(e, errObjectMissing) {
		a.retryTransfer(t.TaskID, e)
		return
	}
	checkpoint := func(uploadID string) error {
		return a.storeTransfer(t.TaskID, func(v *S3Result) { v.UploadID = uploadID })
	}
	if s.UploadID != "" {
		if abortErr := a.remote.Abort(ctx, s.Key, s.UploadID); abortErr != nil {
			a.retryTransfer(t.TaskID, abortErr)
			return
		}
		if checkpoint("") != nil {
			return
		}
		o.UploadID = ""
	}
	if errors.Is(e, errObjectMissing) {
		if e = a.remote.Put(ctx, o, f.File, checkpoint); e != nil {
			a.retryTransfer(t.TaskID, e)
			return
		}
		e = a.remote.Check(ctx, o)
		if errors.Is(e, errObjectConflict) {
			e = errRemoteIntegrity
		}
		if e != nil {
			a.retryTransfer(t.TaskID, e)
			return
		}
	}
	if e = ctx.Err(); e != nil {
		a.retryTransfer(t.TaskID, e)
		return
	}
	_ = a.storeTransfer(t.TaskID, func(v *S3Result) {
		now := time.Now().UTC()
		v.State = "uploaded"
		v.UploadedAt = &now
		v.UploadID = ""
		v.NextAttemptAt = nil
		v.LastErrorCode = ""
	})
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
