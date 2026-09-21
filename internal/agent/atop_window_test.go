package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAtopHelperProcess(t *testing.T) {
	mode := ""
	for _, a := range os.Args {
		if strings.HasPrefix(a, "atop-test-") {
			mode = a
		}
	}
	if mode == "" {
		return
	}
	value, ok := os.LookupEnv("ATOPACCT")
	if !ok || value != "" || os.Getenv("LC_ALL") != "C" {
		os.Exit(3)
	}
	os.Stderr.WriteString("STDERR_SECRET\n")
	switch mode {
	case "atop-test-good":
		os.Stdout.WriteString(atopFixture)
	case "atop-test-hang":
		os.Stdout.WriteString(atopFixture)
		time.Sleep(20 * time.Second)
	case "atop-test-bad":
		os.Stdout.WriteString("INVALID_STDOUT_SECRET\n")
		time.Sleep(20 * time.Second)
	}
	os.Exit(0)
}
func TestAtopExecutableStreamingAndCancellation(t *testing.T) {
	for _, mode := range []string{"good", "hang", "bad", "sink"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Atop.Binary = os.Args[0]
			cfg.Atop.StartupGrace = 0
			cfg.Atop.FinishGrace = 0
			if mode == "good" {
				cfg.Atop.FinishGrace = 2 * time.Second
			}
			c := NewAtopCollector(cfg)
			c.available = nil
			helper := mode
			if helper == "sink" {
				helper = "hang"
			}
			c.run = func(ctx context.Context, _ []string, emit func(Record) error, s WindowSpec) error {
				return c.runStream(ctx, []string{"-test.run=^TestAtopHelperProcess$", "--", "atop-test-" + helper}, emit, s)
			}
			start := time.Now()
			frames := 0
			sentinel := errors.New("sink budget exhausted")
			e := c.CollectWindow(context.Background(), WindowSpec{Window: time.Second, Step: time.Second, Scenes: []string{"cpu"}}, func(r Record) error {
				if strings.Contains(r.Content, "SECRET") {
					t.Fatal("secret persisted")
				}
				if mode == "sink" {
					return sentinel
				}
				if r.Kind == "frame_end" {
					frames++
				}
				return nil
			})
			if time.Since(start) > 4*time.Second {
				t.Fatal("child cleanup exceeded bound")
			}
			switch mode {
			case "good":
				if e != nil || frames != 2 {
					t.Fatalf("%d %v", frames, e)
				}
			case "hang":
				if !errors.Is(e, context.DeadlineExceeded) || frames != 2 {
					t.Fatalf("%d %v", frames, e)
				}
			case "bad":
				if !errors.Is(e, errAtopFormat) {
					t.Fatal(e)
				}
			case "sink":
				if !errors.Is(e, sentinel) {
					t.Fatal(e)
				}
			}
		})
	}
}
func TestAtopUnavailableAndLimits(t *testing.T) {
	cfg := testConfig(t)
	c := NewAtopCollector(cfg)
	if c.CollectWindow(context.Background(), WindowSpec{}, func(Record) error { return nil }) == nil {
		t.Fatal("unverified atop accepted")
	}
	for _, mode := range []string{"disabled", "raw", "path"} {
		v := cfg
		switch mode {
		case "disabled":
			v.Atop.Enabled = false
		case "raw":
			v.Atop.Mode = "raw"
		case "path":
			v.Atop.Binary = "/tmp/atop"
		}
		col := NewAtopCollector(v)
		col.Probe(context.Background())
		if col.Available() == nil {
			t.Fatal("invalid configuration available")
		}
	}
	c.available = nil
	if c.CollectWindow(context.Background(), WindowSpec{Step: time.Millisecond, Window: time.Second}, func(Record) error { return nil }) == nil {
		t.Fatal("fractional step accepted")
	}
	b := &limitedVersionBuffer{}
	if _, e := b.Write([]byte("Version: 2.7.1")); e != nil {
		t.Fatal(e)
	}
	if _, e := b.Write(make([]byte, 4096)); e == nil {
		t.Fatal("unbounded version output")
	}
}

func TestAtopConfigurationBounds(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.Sampling.MaxSourceBytes = 255 },
		func(c *Config) { c.Background.Enabled = true; c.Background.Step = time.Millisecond },
		func(c *Config) { c.Background.Enabled = true; c.Background.Step = 11 * time.Minute },
	} {
		c := testConfig(t)
		change(&c)
		if c.Validate() == nil {
			t.Fatal("unusable atop configuration accepted")
		}
	}
}
