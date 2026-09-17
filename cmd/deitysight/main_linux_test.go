//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCLIEndToEnd(t *testing.T) {
	if os.Getenv("DEITYSIGHT_CLI_TEST") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCLIEndToEnd$")
		cmd.Env = append(os.Environ(), "DEITYSIGHT_CLI_TEST=1")
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("%v\n%s", e, b)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("Linux CLI integration requires root")
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := l.Addr().String()
	l.Close()
	root := t.TempDir()
	cfg := fmt.Sprintf("http:\n  listen: %q\n  token: integration-test-only\nstorage:\n  path: %q\n  min_free_bytes: 1\n", addr, root+"/data")
	if e = os.WriteFile(root+"/agent.yaml", []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	args(t, "--config", root+"/agent.yaml")
	done := make(chan error, 1)
	go func() { done <- run() }()
	client := &http.Client{Timeout: 2 * time.Second}
	call := func(method, path, body string) (int, []byte, error) {
		r, e := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
		if e != nil {
			return 0, nil, e
		}
		r.Header.Set("Authorization", "Bearer integration-test-only")
		res, e := client.Do(r)
		if e != nil {
			return 0, nil, e
		}
		defer res.Body.Close()
		b, e := io.ReadAll(res.Body)
		return res.StatusCode, b, e
	}
	ready := false
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); {
		select {
		case e := <-done:
			t.Fatalf("CLI exited early: %v", e)
		default:
		}
		if status, _, e := call("GET", "/v1/health", ""); e == nil && status == 200 {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("CLI did not start")
	}
	defer func() {
		if e := syscall.Tgkill(os.Getpid(), syscall.Gettid(), syscall.SIGTERM); e != nil {
			t.Error(e)
		}
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(15 * time.Second):
			t.Error("CLI shutdown timed out")
		}
	}()
	status, b, e := call("POST", "/v1/tasks", `{"request_id":"cli-e2e","window_seconds":1,"step_seconds":1}`)
	if e != nil || status != 202 {
		t.Fatalf("submit %d %s %v", status, b, e)
	}
	var task struct {
		TaskID string `json:"task_id"`
		State  string `json:"state"`
	}
	if e = json.Unmarshal(b, &task); e != nil {
		t.Fatal(e)
	}
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); {
		status, b, e = call("GET", "/v1/tasks/"+task.TaskID, "")
		if e != nil || status != 200 {
			t.Fatalf("poll %d %v", status, e)
		}
		if e = json.Unmarshal(b, &task); e != nil {
			t.Fatal(e)
		}
		if task.State != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	status, b, e = call("GET", "/v1/tasks/"+task.TaskID+"/result", "")
	if e != nil || status != 200 || len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatalf("download %d %v", status, e)
	}
}
