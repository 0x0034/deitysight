//go:build yosintegration

package agent

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Explicit opt-in: writes one synthetic task archive to the allocated namespace
// and deletes only that task's unique object. Never uploads host evidence.
func TestYOSLiveTransfer(t *testing.T) {
	endpoint, namespace := os.Getenv("DEITYSIGHT_TEST_YOS_ENDPOINT"), os.Getenv("DEITYSIGHT_TEST_YOS_NAMESPACE")
	if endpoint == "" || namespace == "" {
		t.Fatal("set allocated YOS endpoint and namespace")
	}
	c := testConfig(t)
	c.S3.Enabled = true
	c.S3.Provider = "yos"
	c.S3.AllowHTTP = os.Getenv("DEITYSIGHT_TEST_YOS_ALLOW_HTTP") == "true"
	c.S3.Endpoint, c.S3.Bucket = endpoint, namespace
	c.S3.PresignTTL = 5 * time.Minute
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	a := transferAgent(t, c, fixtureCollector{}, nil)
	task, _, err := a.Submit(Request{RequestID: "yos-live-" + uuid()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		a.Close()
		// Read authoritative task metadata without making another signing request.
		a.mu.Lock()
		saved := a.tasks[task.TaskID].clone()
		a.mu.Unlock()
		if saved.Result.S3 == nil || saved.Result.S3.Key == "" {
			return
		}
		u := c.S3.target().Endpoint + "/" + namespace + "/" + saved.Result.S3.Key
		hc := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		r, _ := http.NewRequest(http.MethodDelete, u, nil)
		response, err := hc.Do(r)
		if err != nil {
			t.Error("cleanup request failed; retained key:", saved.Result.S3.Key)
			return
		}
		response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 204 {
			t.Error("cleanup failed", response.StatusCode, saved.Result.S3.Key)
			return
		}
		r, _ = http.NewRequest(http.MethodHead, u, nil)
		response, err = hc.Do(r)
		if err != nil {
			t.Error("cleanup verification failed")
			return
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Error("test object still present", saved.Result.S3.Key)
		}
	}()
	var got Task
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		got, err = a.Get(task.TaskID)
		if err != nil {
			t.Fatal("task query failed")
		}
		if got.Result.S3.State == "uploaded" {
			break
		}
		if got.Result.S3.State == "retry_wait" || got.Result.S3.State == "unavailable" {
			t.Fatal("transfer failed", got.Result.S3.State, got.Result.S3.LastErrorCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.Result.S3.State != "uploaded" || got.Result.S3.URL == "" {
		t.Fatal("missing uploaded result", got.Result.S3.State, got.Result.S3.URLErrorCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, got.Result.S3.URL, nil)
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := hc.Do(r)
	if err != nil {
		t.Fatal("COS signed download failed")
	}
	defer response.Body.Close()
	n, sum, err := digest(io.LimitReader(response.Body, got.Result.Size+1))
	if err != nil || response.StatusCode != 200 || n != got.Result.Size || sum != got.Result.SHA256 {
		t.Fatal("COS signed download integrity mismatch")
	}
	t.Logf("YOS task uploaded; COS HTTPS download verified: bytes=%d sha256=%s", n, sum)
}
