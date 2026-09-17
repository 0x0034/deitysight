//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestLiveLinuxCollection(t *testing.T) {
	c, e := NewLinuxCollector("/proc", 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	counts := map[string]int{}
	sources := map[string]bool{}
	errors := map[string]int{}
	bytes := 0
	start := time.Now()
	e = c.Collect(ctx, func(r Record) error {
		counts[r.Scope]++
		sources[r.Source] = true
		bytes += len(r.Content)
		if r.Code != "" {
			errors[r.Code]++
		}
		return nil
	})
	if e != nil {
		t.Fatalf("live collection: %v", e)
	}
	if !sources["/proc/stat"] || !sources[fmt.Sprintf("/proc/%d/io", os.Getpid())] || counts["thread"] == 0 || counts["cgroup"] == 0 {
		t.Fatalf("missing coverage: %v", counts)
	}
	if c.Metadata()["boot_id"] == nil || c.Metadata()["clock_ticks"] == nil {
		t.Fatalf("metadata unavailable: %v", c.Metadata())
	}
	t.Logf("live duration=%s bytes=%d scope_counts=%v source_errors=%v", time.Since(start), bytes, counts, errors)
}
