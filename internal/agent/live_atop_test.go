//go:build linux

package agent

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestLiveAtopCollection(t *testing.T) {
	if _, err := exec.LookPath("/usr/bin/atop"); err != nil {
		t.Skip("atop is not installed in this Linux test environment")
	}
	c, err := NewLinuxCollectorWithAtop("/proc", 1<<20, AtopConfig{Enabled: true, Binary: "/usr/bin/atop", Interval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	found := false
	if err := c.collectAtop(withTaskSampling(ctx, 30*time.Second, 5*time.Second), func(r Record) error {
		found = r.Source == "/atop/parseable" && (r.Kind == "source" || r.Kind == "error")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("atop record was not emitted")
	}
}
