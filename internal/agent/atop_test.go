package agent

import (
	"context"
	"errors"
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
	if err := c.collectAtop(context.Background(), func(r Record) error { got = append(got, r); return nil }); err != nil {
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
		if len(args) != 4 || args[0] != "-P" || args[1] != "ALL" || args[2] != "1" || args[3] != "1" {
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
	if err := c.collectAtop(context.Background(), func(r Record) error { got = append(got, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if !called || len(got) != 1 || got[0].Kind != "error" || got[0].Code != "atop_failed" || !strings.Contains(got[0].Content, "atop failed") {
		t.Fatalf("atop failure not preserved: called=%v records=%+v", called, got)
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
}
