package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRemote struct {
	mu      sync.Mutex
	objects map[string]remoteObject
	data    map[string][]byte
	puts    int
	failure error
	block   chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{objects: map[string]remoteObject{}, data: map[string][]byte{}}
}
func (f *fakeRemote) Head(ctx context.Context, key string) (remoteObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure != nil {
		return remoteObject{}, f.failure
	}
	v, ok := f.objects[key]
	if !ok {
		return remoteObject{}, errObjectMissing
	}
	return v, nil
}
func (f *fakeRemote) Put(ctx context.Context, o uploadObject, file *os.File, checkpoint func(string) error) error {
	if f.entered != nil {
		f.once.Do(func() { close(f.entered) })
	}
	if f.block != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.block:
		}
	}
	b, e := io.ReadAll(file)
	if e != nil {
		return e
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.failure != nil {
		return f.failure
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != o.SHA256 {
		return errors.New("unexpected upload bytes")
	}
	f.objects[o.Key] = remoteObject{Size: int64(len(b)), SHA256: o.SHA256, AgentID: o.AgentID, TaskID: o.TaskID}
	f.data[o.Key] = b
	return nil
}
func (f *fakeRemote) Abort(context.Context, string, string) error { return nil }
func (f *fakeRemote) Presign(context.Context, string, time.Duration) (string, error) {
	return "https://s3.example.test/bucket/result?X-Amz-Signature=temporary-fixture", nil
}
func transferConfig(t *testing.T) Config {
	c := testConfig(t)
	c.S3.Enabled = true
	c.S3.Endpoint = "https://s3.example.test"
	c.S3.Region = "us-east-1"
	c.S3.Bucket = "test-bucket"
	c.S3.AccessKeyID = "fixture-access"
	c.S3.SecretAccessKey = "fixture-secret-not-real"
	c.S3.RetryInitial = 20 * time.Millisecond
	c.S3.RetryMax = 50 * time.Millisecond
	return c
}
func transferAgent(t *testing.T, c Config, f Collector, r remoteStore) *Agent {
	t.Helper()
	a, e := newAgent(c, f, r)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(a.Close)
	return a
}
func waitTransfer(t *testing.T, a *Agent, id, state string) Task {
	t.Helper()
	end := time.Now().Add(6 * time.Second)
	for time.Now().Before(end) {
		v, e := a.Get(id)
		if e != nil {
			t.Fatal(e)
		}
		if v.Result.S3 != nil && v.Result.S3.State == state {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	v, _ := a.Get(id)
	t.Fatalf("waiting %s: %+v", state, v.Result.S3)
	return Task{}
}

func TestS3UploadAndIndependentLocalExpiry(t *testing.T) {
	c := transferConfig(t)
	r := newFakeRemote()
	a := transferAgent(t, c, fixtureCollector{}, r)
	task, _, e := a.Submit(Request{RequestID: "remote-result"})
	if e != nil {
		t.Fatal(e)
	}
	got := waitTransfer(t, a, task.TaskID, "uploaded")
	if got.State != "completed" || got.Result.S3.URL == "" || got.Result.S3.URLExpiresAt == nil {
		t.Fatalf("%+v", got)
	}
	local := request(t, a, "GET", "/v1/tasks/"+task.TaskID+"/result", "", c.HTTP.Token)
	r.mu.Lock()
	remote := r.data[got.Result.S3.Key]
	r.mu.Unlock()
	if !bytes.Equal(local.Body.Bytes(), remote) {
		t.Fatal("S3 bytes differ from local download")
	}
	persisted, e := os.ReadFile(filepath.Join(c.Storage.Path, taskPath(task.TaskID, "task.json")))
	if e != nil {
		t.Fatal(e)
	}
	files := archiveFiles(t, remote)
	for _, v := range [][]byte{persisted, files["manifest.json"]} {
		for _, secret := range []string{c.S3.SecretAccessKey, c.S3.AccessKeyID, "temporary-fixture", "X-Amz-Signature"} {
			if bytes.Contains(v, []byte(secret)) {
				t.Fatal("credential/link persisted")
			}
		}
	}
	a.mu.Lock()
	v := a.tasks[task.TaskID]
	past := time.Now().Add(-time.Second)
	v.ResultExpiresAt = &past
	a.tasks[v.TaskID] = v
	if e = a.persist(v); e != nil {
		t.Fatal(e)
	}
	a.mu.Unlock()
	a.cleanup()
	got, e = a.Get(task.TaskID)
	if e != nil || got.Result.Available || !got.Result.Expired || got.Result.S3.URL == "" {
		t.Fatal("local expiry broke remote access")
	}
	if w := request(t, a, "GET", "/v1/tasks/"+task.TaskID+"/result", "", c.HTTP.Token); w.Code != 410 {
		t.Fatal(w.Code)
	}
	if _, yes, e := a.Submit(Request{RequestID: "remote-result"}); e != nil || !yes {
		t.Fatal("idempotency lost")
	}
	a.Close()
	b := transferAgent(t, c, fixtureCollector{}, r)
	got, e = b.Get(task.TaskID)
	if e != nil || got.Result.S3.State != "uploaded" || got.Result.S3.URL == "" {
		t.Fatal("remote availability lost after restart")
	}
}

func TestS3FailureRetryPauseAndNoBackfill(t *testing.T) {
	c := transferConfig(t)
	r := newFakeRemote()
	r.failure = errors.New("DO_NOT_EXPOSE_PROVIDER_SECRET")
	a := transferAgent(t, c, fixtureCollector{fail: true}, r)
	task, _, e := a.Submit(Request{RequestID: "retry"})
	if e != nil {
		t.Fatal(e)
	}
	got := waitTransfer(t, a, task.TaskID, "retry_wait")
	if got.State != "partial" || !got.Result.Available || got.Result.S3.URL != "" {
		t.Fatal("upload failure altered collection")
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "DO_NOT_EXPOSE") {
		t.Fatal("provider error leaked")
	}
	a.Close()
	paused := c
	paused.S3.Enabled = false
	bAgent := transferAgent(t, paused, fixtureCollector{}, r)
	got, e = bAgent.Get(task.TaskID)
	if e != nil || !got.Result.S3.Paused {
		t.Fatal("disabled upload not paused")
	}
	bAgent.Close()
	r.mu.Lock()
	r.failure = nil
	r.mu.Unlock()
	resumed := transferAgent(t, c, fixtureCollector{}, r)
	waitTransfer(t, resumed, task.TaskID, "uploaded")
	resumed.Close()
	c2 := testConfig(t)
	old := openTestAgent(t, c2, fixtureCollector{})
	oldTask, _, e := old.Submit(Request{RequestID: "old"})
	if e != nil {
		t.Fatal(e)
	}
	terminal(t, old, oldTask.TaskID)
	old.Close()
	c2.S3 = c.S3
	newInstance := transferAgent(t, c2, fixtureCollector{}, r)
	v, e := newInstance.Get(oldTask.TaskID)
	if e != nil || v.Result.S3 != nil {
		t.Fatal("unexpected backfill")
	}
}

func TestS3UploadDoesNotBlockSamplingAndCancelsOnShutdown(t *testing.T) {
	c := transferConfig(t)
	r := newFakeRemote()
	r.block = make(chan struct{})
	r.entered = make(chan struct{})
	a := transferAgent(t, c, fixtureCollector{}, r)
	first, _, e := a.Submit(Request{RequestID: "slow-upload"})
	if e != nil {
		t.Fatal(e)
	}
	terminal(t, a, first.TaskID)
	select {
	case <-r.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upload not started")
	}
	next, _, e := a.Submit(Request{RequestID: "next-sample"})
	if e != nil {
		t.Fatal(e)
	}
	if terminal(t, a, next.TaskID).State != "completed" {
		t.Fatal("upload blocked collection")
	}
	start := time.Now()
	a.Close()
	if time.Since(start) > time.Second {
		t.Fatal("shutdown failed to cancel upload")
	}
	close(r.block)
	b := transferAgent(t, c, fixtureCollector{}, r)
	waitTransfer(t, b, first.TaskID, "uploaded")
	waitTransfer(t, b, next.TaskID, "uploaded")
}

func TestS3ConfigurationValidation(t *testing.T) {
	c := transferConfig(t)
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*S3Config){func(s *S3Config) { s.Endpoint = "http://s3.example.test" }, func(s *S3Config) { s.Endpoint = "https://user:password@example.test" }, func(s *S3Config) { s.Endpoint += "?token=bad" }, func(s *S3Config) { s.Region = "" }, func(s *S3Config) { s.Bucket = "../outside" }, func(s *S3Config) { s.Prefix = "a/../b" }, func(s *S3Config) { s.SecretAccessKey = "REPLACE_WITH_SECRET" }, func(s *S3Config) { s.PresignTTL = 8 * 24 * time.Hour }, func(s *S3Config) { s.RetryMax = time.Nanosecond }} {
		bad := c
		mutate(&bad.S3)
		if bad.Validate() == nil {
			t.Fatal("invalid S3 configuration accepted")
		}
	}
	c.S3.Enabled = false
	c.S3.Endpoint = ""
	if e := c.Validate(); e != nil {
		t.Fatal("disabled S3 rejected legacy config")
	}
}
