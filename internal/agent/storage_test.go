package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreAccountingReservationAndLease(t *testing.T) {
	cfg := testConfig(t).Storage
	s, e := openStore(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Atomic("sample", []byte("12345")); e != nil {
		t.Fatal(e)
	}
	if e = s.Atomic("sample", []byte("12")); e != nil {
		t.Fatal(e)
	}
	if s.used != 2 {
		t.Fatalf("replace accounting: %d", s.used)
	}
	if e = s.BeginCapture(); e != nil {
		t.Fatal(e)
	}
	if e = s.Append("capture", []byte("abc"), true, false); e != nil {
		t.Fatal(e)
	}
	if s.reserve != 4102 {
		t.Fatalf("no archive reservation: %d", s.reserve)
	}
	if e = s.Append("archive.tmp", []byte("xyz"), false, true); e != nil {
		t.Fatal(e)
	}
	if e = s.Publish("archive.tmp", "archive"); e != nil {
		t.Fatal(e)
	}
	f, e := s.Lease("archive")
	if e != nil {
		t.Fatal(e)
	}
	used := s.used
	if e = s.Remove("archive"); e != nil {
		t.Fatal(e)
	}
	if s.used != used {
		t.Fatal("pinned download excluded from quota")
	}
	if e = s.Publish("sample", "archive"); e == nil {
		t.Fatal("overwrote pinned archive")
	}
	if _, e = io.ReadAll(f); e != nil {
		t.Fatal(e)
	}
	f.Close()
	if s.used != used-3 {
		t.Fatal("lease release not accounted")
	}
	if e = s.Publish("capture", "sample"); e != nil {
		t.Fatal(e)
	}
	if e = s.Remove("missing"); e != nil {
		t.Fatal(e)
	}
	s.EndCapture()
	s.Close()
	if s.Available() == nil {
		t.Fatal("closed store available")
	}
}

func TestStoragePathConfinementAndFreeSpace(t *testing.T) {
	c := testConfig(t)
	s, e := openStore(c.Storage)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Atomic("../escape", []byte("forbidden")); e == nil {
		t.Fatal("storage escaped root")
	}
	if e = s.Mkdir("nested"); e != nil {
		t.Fatal(e)
	}
	for _, op := range []func() error{func() error { return s.Atomic("nested", nil) }, func() error { return s.Append("nested", nil, false, false) }, func() error { _, e := s.Open("nested"); return e }, func() error { return s.Remove("nested") }, func() error { _, e := s.Lease("nested"); return e }} {
		if op() == nil {
			t.Fatal("non-regular data accepted")
		}
	}
	if s.RemoveTask("../escape") == nil {
		t.Fatal("invalid task path")
	}
	s.cfg.MinFreeBytes = ^uint64(0)
	if s.Available() == nil {
		t.Fatal("free space floor ignored")
	}
	c2 := testConfig(t)
	outside := t.TempDir()
	if e = os.Symlink(outside, c2.Storage.Path); e != nil {
		t.Fatal(e)
	}
	if x, e := openStore(c2.Storage); e == nil {
		x.Close()
		t.Fatal("symlink storage root accepted")
	}
	c3 := testConfig(t)
	fixtureWrite(t, c3.Storage.Path, "sample", []byte("bytes"))
	if e = os.Symlink("sample", filepath.Join(c3.Storage.Path, "bad-link")); e != nil {
		t.Fatal(e)
	}
	if x, e := openStore(c3.Storage); e == nil {
		x.Close()
		t.Fatal("symlink in storage accepted")
	}
}

func TestDuplicateRequestMetadataBlocksAdmission(t *testing.T) {
	c := testConfig(t)
	a := openTestAgent(t, c, fixtureCollector{})
	task, _, e := a.Submit(Request{RequestID: "duplicate"})
	if e != nil {
		t.Fatal(e)
	}
	got := terminal(t, a, task.TaskID)
	a.Close()
	got.TaskID = uuid()
	b, _ := jsonBytes(got)
	fixtureWrite(t, c.Storage.Path, taskPath(got.TaskID, "task.json"), b)
	d := openTestAgent(t, c, fixtureCollector{})
	if _, _, e = d.Submit(Request{RequestID: "different"}); e == nil {
		t.Fatal("conflicting recovery accepted new work")
	}
	if _, _, e = d.Submit(Request{RequestID: "duplicate"}); e == nil {
		t.Fatal("ambiguous duplicate request was replayed")
	}
	if _, e = d.Get(task.TaskID); e != nil {
		t.Fatal("valid existing task unavailable")
	}
}

func TestConfigurationLimitsAndSourceErrorPaths(t *testing.T) {
	c := testConfig(t)
	for _, mutate := range []func(*Config){func(c *Config) { c.HTTP.Listen = "invalid" }, func(c *Config) { c.Storage.Path = "relative" }, func(c *Config) { c.Storage.Path = "/proc/agent" }, func(c *Config) { c.Background.Step = 0 }, func(c *Config) { c.Sampling.MaxSourceBytes = 17 << 20 }, func(c *Config) { c.Sampling.MaxPoints = 2 }} {
		v := c
		mutate(&v)
		if v.Sampling.MaxPoints == 2 {
			v.Sampling.DefaultWindow = 3 * time.Second
		}
		if v.Validate() == nil {
			t.Fatal("invalid config accepted")
		}
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	fixtureWrite(t, filepath.Dir(p), "config.yaml", []byte("http:\n  token: test-token\n---\nhttp:\n  token: test-token\n"))
	if _, e := LoadConfig(p); e == nil {
		t.Fatal("multiple YAML documents accepted")
	}
	for _, b := range []string{"", "42", "42 (x) S x", "42 (x) S " + strings.Repeat("0 ", 18) + "bad"} {
		if startTime([]byte(b)) != "" {
			t.Fatal("invalid identity accepted")
		}
	}
	for _, e := range []error{os.ErrPermission, os.ErrNotExist, context.DeadlineExceeded, errTruncated, io.ErrUnexpectedEOF} {
		if sourceCode(e) == "" {
			t.Fatal("unclassified error")
		}
	}
	if Schedule(0, time.Second) != nil {
		t.Fatal("zero window schedule")
	}
	if validID(strings.Repeat("z", 36)) || validID("00000000_0000-0000-0000-000000000000") {
		t.Fatal("invalid id accepted")
	}
	if _, e := New(c, nil); e == nil {
		t.Fatal("nil collector accepted")
	}
	if e := readRecords(strings.NewReader("broken\n"), 1024, func(Record) error { return nil }); e == nil {
		t.Fatal("corrupt record accepted")
	}
	sentinel := errors.New("sink")
	b, _ := jsonBytes(Record{})
	if e := readRecords(strings.NewReader(string(b)), 1024, func(Record) error { return sentinel }); !errors.Is(e, sentinel) {
		t.Fatal("record sink error swallowed")
	}
}
