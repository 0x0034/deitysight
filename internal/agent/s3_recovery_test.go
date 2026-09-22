package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestS3UploadedObjectIsReconciledAfterLostAcknowledgement(t *testing.T) {
	c := transferConfig(t)
	r := newFakeRemote()
	a := transferAgent(t, c, fixtureCollector{}, r)
	task, _, e := a.Submit(Request{RequestID: "lost-ack"})
	if e != nil {
		t.Fatal(e)
	}
	got := waitTransfer(t, a, task.TaskID, "uploaded")
	a.Close()
	got.Result.S3.State = "uploading"
	got.Result.S3.UploadedAt = nil
	got.Result.S3.URL = ""
	got.Result.S3.URLExpiresAt = nil
	raw, _ := jsonBytes(got)
	fixtureWrite(t, c.Storage.Path, taskPath(task.TaskID, "task.json"), raw)
	b := transferAgent(t, c, fixtureCollector{}, r)
	waitTransfer(t, b, task.TaskID, "uploaded")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.puts != 1 {
		t.Fatalf("reconciliation re-uploaded object: %d", r.puts)
	}
}
func TestS3TargetChangeAndCredentialRotation(t *testing.T) {
	c := transferConfig(t)
	r := newFakeRemote()
	a := transferAgent(t, c, fixtureCollector{}, r)
	task, _, e := a.Submit(Request{RequestID: "destination"})
	if e != nil {
		t.Fatal(e)
	}
	got := waitTransfer(t, a, task.TaskID, "uploaded")
	a.Close()
	changed := c
	changed.S3.Bucket = "different-bucket"
	b := transferAgent(t, changed, fixtureCollector{}, r)
	v, e := b.Get(task.TaskID)
	if e != nil || v.Result.S3.URL != "" || v.Result.S3.URLErrorCode != "s3_target_changed" || v.Result.S3.Bucket != got.Result.S3.Bucket {
		t.Fatal("silently migrated existing target")
	}
	b.Close()
	c.S3.SecretAccessKey = "rotated-fixture-secret"
	d := transferAgent(t, c, fixtureCollector{}, r)
	v, e = d.Get(task.TaskID)
	if e != nil || v.Result.S3.URL == "" {
		t.Fatal("credential rotation changed object identity")
	}
}
func TestS3ExpiryCancelsUploadAndRetainsDownloadContract(t *testing.T) {
	c := transferConfig(t)
	c.Storage.ResultRetention = 300 * time.Millisecond
	r := newFakeRemote()
	r.entered = make(chan struct{})
	r.block = make(chan struct{})
	a := transferAgent(t, c, fixtureCollector{}, r)
	task, _, e := a.Submit(Request{RequestID: "deadline"})
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-r.entered:
	case <-time.After(4 * time.Second):
		t.Fatal("upload not started")
	}
	waitTransfer(t, a, task.TaskID, "expired")
	a.cleanup()
	if w := request(t, a, "GET", "/v1/tasks/"+task.TaskID+"/result", "", c.HTTP.Token); w.Code != 410 {
		t.Fatal(w.Code)
	}
	a.Close()
	if _, e = os.Stat(filepath.Join(c.Storage.Path, taskPath(task.TaskID, "result.tar.gz"))); !os.IsNotExist(e) {
		t.Fatalf("expired upload lease not released: %v", e)
	}
	b := transferAgent(t, c, fixtureCollector{}, r)
	v, e := b.Get(task.TaskID)
	if e != nil || v.Result.S3.State != "expired" {
		t.Fatal("expiry not preserved on restart")
	}
}
func TestS3InterruptedTaskAndFailedEvidenceAreUploaded(t *testing.T) {
	c := transferConfig(t)
	r := newFakeRemote()
	a := transferAgent(t, c, fixtureCollector{}, r)
	task, _, e := a.Submit(Request{RequestID: "interrupted-s3"})
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(60 * time.Millisecond)
	a.Close()
	b := transferAgent(t, c, fixtureCollector{}, r)
	v := waitTransfer(t, b, task.TaskID, "uploaded")
	if v.State != "interrupted" {
		t.Fatal(v.State)
	}
	b.Close()
	r2 := newFakeRemote()
	d := transferAgent(t, transferConfig(t), emptyCollector{}, r2)
	task, _, e = d.Submit(Request{RequestID: "failed-evidence"})
	if e != nil {
		t.Fatal(e)
	}
	v = waitTransfer(t, d, task.TaskID, "uploaded")
	if v.State != "failed" || !v.Result.Available {
		t.Fatal("failed evidence package was excluded")
	}
}

type emptyCollector struct{}

func (emptyCollector) Metadata() map[string]any                          { return map[string]any{} }
func (emptyCollector) Collect(context.Context, func(Record) error) error { return nil }

func TestS3UnavailableArchiveAndObjectConflict(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "conflict", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			c := transferConfig(t)
			r := newFakeRemote()
			a := transferAgent(t, c, fixtureCollector{}, r)
			task, _, e := a.Submit(Request{RequestID: mode})
			if e != nil {
				t.Fatal(e)
			}
			got := waitTransfer(t, a, task.TaskID, "uploaded")
			a.Close()
			got.Result.S3.State = "pending"
			got.Result.S3.UploadedAt = nil
			got.Result.S3.URL = ""
			got.Result.S3.URLExpiresAt = nil
			raw, _ := jsonBytes(got)
			fixtureWrite(t, c.Storage.Path, taskPath(task.TaskID, "task.json"), raw)
			switch mode {
			case "missing":
				if e = os.Remove(filepath.Join(c.Storage.Path, taskPath(task.TaskID, "result.tar.gz"))); e != nil {
					t.Fatal(e)
				}
			case "corrupt":
				fixtureWrite(t, c.Storage.Path, taskPath(task.TaskID, "result.tar.gz"), []byte("corrupt"))
			case "conflict":
				r.objects[got.Result.S3.Key] = remoteObject{Size: got.Result.Size, SHA256: strings.Repeat("f", 64)}
			case "forbidden":
				r.failure = errors.New("access denied")
			}
			b := transferAgent(t, c, fixtureCollector{}, r)
			state := "retry_wait"
			if mode == "missing" || mode == "corrupt" {
				state = "unavailable"
			}
			v := waitTransfer(t, b, task.TaskID, state)
			if v.Result.S3.URL != "" || v.State != "completed" {
				t.Fatal("bad remote state")
			}
			r.mu.Lock()
			puts := r.puts
			r.mu.Unlock()
			if puts != 1 {
				t.Fatal("overwrote remote object")
			}
		})
	}
}

func TestS3SigningFailureDoesNotEraseUploadedState(t *testing.T) {
	c := transferConfig(t)
	r := newFakeRemote()
	a := transferAgent(t, c, fixtureCollector{}, r)
	task, _, e := a.Submit(Request{RequestID: "sign-failure"})
	if e != nil {
		t.Fatal(e)
	}
	waitTransfer(t, a, task.TaskID, "uploaded")
	a.Close()
	b := transferAgent(t, c, fixtureCollector{}, signFailureRemote{r})
	v, e := b.Get(task.TaskID)
	if e != nil || v.Result.S3.State != "uploaded" || v.Result.S3.URL != "" || v.Result.S3.URLErrorCode != "signing_failed" {
		t.Fatal("signing failure affected durable upload outcome")
	}
}

type signFailureRemote struct{ *fakeRemote }

func (signFailureRemote) Presign(context.Context, string, time.Duration) (string, error) {
	return "", errors.New("private signing details")
}

func TestS3MalformedUploadIntentBlocksRecovery(t *testing.T) {
	c := transferConfig(t)
	r := newFakeRemote()
	a := transferAgent(t, c, fixtureCollector{}, r)
	task, _, e := a.Submit(Request{RequestID: "bad-intent"})
	if e != nil {
		t.Fatal(e)
	}
	v := waitTransfer(t, a, task.TaskID, "uploaded")
	a.Close()
	v.Result.S3.State = "pending"
	v.Result.S3.Key = ""
	v.Result.S3.URL = ""
	v.Result.S3.URLExpiresAt = nil
	b, _ := json.Marshal(v)
	fixtureWrite(t, c.Storage.Path, taskPath(v.TaskID, "task.json"), b)
	recovered := transferAgent(t, c, fixtureCollector{}, r)
	if _, _, e = recovered.Submit(Request{RequestID: "new-task"}); e == nil {
		t.Fatal("corrupt S3 intent did not block admission")
	}
}
