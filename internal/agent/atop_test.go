package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAtopCollectorEmitsRawParseableSnapshot(t *testing.T) {
	runner := func(context.Context, []string) ([]byte, []byte, error) {
		return []byte("RESET 20260918 120000\nCPU 1 2 3\nPRC 4 5 6\n"), nil, nil
	}
	c, err := NewLinuxCollectorWithAtopRunner(t.TempDir(), 1024, AtopConfig{Enabled: true, Binary: "/usr/bin/atop", Interval: time.Second}, runner)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var got []Record
	if err := c.collectAtop(withTaskSampling(context.Background(), 30*time.Second, 5*time.Second), func(r Record) error { got = append(got, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Source != "/atop/parseable" || got[0].Content != "RESET 20260918 120000\nCPU 1 2 3\nPRC 4 5 6\n" || got[0].Encoding != "utf8" || !got[0].Complete {
		t.Fatalf("unexpected atop record: %+v", got)
	}
}

func TestAtopCollectorDoesNotUseShellAndReportsFailure(t *testing.T) {
	called := false
	runner := func(ctx context.Context, args []string) ([]byte, []byte, error) {
		called = true
		if len(args) != 4 || args[0] != "-P" || args[1] != "ALL" || args[2] != "5" || args[3] != "1" {
			t.Fatalf("unexpected atop args: %v", args)
		}
		return nil, []byte("atop failed"), errors.New("exit status 1")
	}
	c, err := NewLinuxCollectorWithAtopRunner(t.TempDir(), 1024, AtopConfig{Enabled: true, Binary: "/usr/bin/atop", Interval: time.Second}, runner)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var got []Record
	if err := c.collectAtop(withTaskSampling(context.Background(), 30*time.Second, 5*time.Second), func(r Record) error { got = append(got, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if !called || len(got) != 1 || got[0].Kind != "error" || got[0].Code != "atop_failed" || !strings.Contains(got[0].Content, "atop failed") {
		t.Fatalf("atop failure not preserved: called=%v records=%+v", called, got)
	}
}

func TestAtopCollectorEmitsRawWFileAsBase64(t *testing.T) {
	raw := []byte{0x00, 0x01, 0xff, 0x7f, 0x42}
	dir := t.TempDir()
	var outputPath string
	runner := func(ctx context.Context, args []string, path string) ([]byte, error) {
		if len(args) != 3 || args[0] != "-w" || args[1] != "5" || args[2] != "1" {
			t.Fatalf("unexpected atop raw args: %v", args)
		}
		_ = ctx
		outputPath = path
		if !filepath.IsAbs(path) || !strings.HasPrefix(path, dir+string(os.PathSeparator)) {
			t.Fatalf("raw output escaped configured directory: %s", path)
		}
		return nil, os.WriteFile(path, raw, 0600)
	}
	c, err := NewLinuxCollectorWithAtopRawRunner(t.TempDir(), 1024, AtopConfig{Enabled: true, Mode: "raw", Path: dir, Binary: "/usr/bin/atop", Interval: time.Second}, runner)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var got []Record
	if err := c.collectAtop(withTaskSampling(context.Background(), 30*time.Second, 5*time.Second), func(r Record) error { got = append(got, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Source != "/atop/raw" || got[0].Encoding != "base64" || got[0].Content != "AAH/f0I=" || !got[0].Complete {
		t.Fatalf("unexpected raw atop record: %+v", got)
	}
	if outputPath == "" {
		t.Fatal("raw output path was not provided")
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatal("raw output was not removed after collection")
	}
}

func TestAtopIsTaskScoped(t *testing.T) {
	called := false
	c, err := NewLinuxCollectorWithAtopRunner(t.TempDir(), 1024, AtopConfig{Enabled: true, Binary: "/usr/bin/atop", Interval: time.Second}, func(context.Context, []string) ([]byte, []byte, error) {
		called = true
		return []byte("unexpected"), nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.collectAtop(context.Background(), func(Record) error { t.Fatal("background emitted atop"); return nil }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("atop started without a task context")
	}
}

func TestAtopConfigValidation(t *testing.T) {
	c := testConfig(t)
	c.Atop.Enabled = true
	c.Atop.Binary = "/tmp/not-atop"
	if c.Validate() == nil {
		t.Fatal("non-whitelisted atop binary accepted")
	}
	c.Atop.Binary = "/usr/bin/atop"
	c.Atop.Interval = 0
	if c.Validate() == nil {
		t.Fatal("invalid atop interval accepted")
	}
	c.Atop.Interval = time.Second
	c.Atop.Mode = "raw"
	c.Atop.Path = "/var/log/atop"
	if c.Validate() == nil {
		t.Fatal("raw atop path outside storage accepted")
	}
}
