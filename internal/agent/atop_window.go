package agent

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"time"
)

type AtopCollector struct {
	cfg       Config
	available error
	run       func(context.Context, []string, func(Record) error, WindowSpec) error
}

func NewAtopCollector(cfg Config) *AtopCollector {
	c := &AtopCollector{cfg: cfg, available: errors.New("atop 2.7.1 not verified")}
	c.run = c.runStream
	return c
}

// Probe records readiness without preventing the authenticated HTTP API from starting.
func (c *AtopCollector) Probe(ctx context.Context) {
	if !c.cfg.Atop.Enabled || c.cfg.Atop.Mode != "parseable" || !allowedAtopBinary(c.cfg.Atop.Binary) {
		c.available = errors.New("atop configuration unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := atopCommand(ctx, c.cfg.Atop.Binary, "-V")
	b := &limitedVersionBuffer{}
	cmd.Stdout = b
	cmd.Stderr = b
	if err := cmd.Run(); err != nil || !supportedAtopVersion(b.text) {
		c.available = errors.New("atop 2.7.1 required")
		return
	}
	c.available = nil
}

type limitedVersionBuffer struct{ text string }

func (b *limitedVersionBuffer) Write(p []byte) (int, error) {
	if len(b.text)+len(p) > 4096 {
		return 0, errors.New("version output too large")
	}
	b.text += string(p)
	return len(p), nil
}
func atopCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	// A present but empty ATOPACCT prevents all system accounting side effects.
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "TERM=dumb", "ATOPACCT="}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
func (c *AtopCollector) Available() error { return c.available }
func (c *AtopCollector) Metadata() map[string]any {
	return map[string]any{"architecture": runtime.GOARCH, "atop_version": "2.7.1", "atop_mode": "parseable", "atop_enabled": true, "boot_id": nil}
}
func (c *AtopCollector) Collect(ctx context.Context, emit func(Record) error) error {
	return c.CollectWindow(ctx, WindowSpec{Window: c.cfg.Sampling.DefaultWindow, Step: c.cfg.Sampling.DefaultStep, Scenes: allScenes}, emit)
}
func (c *AtopCollector) CollectWindow(ctx context.Context, s WindowSpec, emit func(Record) error) error {
	if c.available != nil {
		return c.available
	}
	if s.Step <= 0 || s.Step%time.Second != 0 || s.Window <= 0 {
		return errors.New("invalid atop window")
	}
	ctx, cancel := context.WithTimeout(ctx, s.Window+c.cfg.Atop.StartupGrace+c.cfg.Atop.FinishGrace)
	defer cancel()
	err := c.run(ctx, s.args(), emit, s)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
func (c *AtopCollector) runStream(ctx context.Context, args []string, emit func(Record) error, s WindowSpec) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := atopCommand(ctx, c.cfg.Atop.Binary, args...)
	// Raw stderr could contain process data or command arguments. Never persist it.
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	parseErr := ParseAtopStream(stdout, s, int(c.cfg.Sampling.MaxSourceBytes), func(r Record) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		return emit(r)
	})
	if parseErr != nil {
		cancel()
		_ = stdout.Close()
	}
	waitErr := cmd.Wait()
	if parseErr != nil {
		return parseErr
	}
	return waitErr
}

func supportedAtopVersion(s string) bool {
	return regexp.MustCompile(`(?im)^version:?[ \t]+2\.7\.1(?:[ \t\r\n]|$)`).MatchString(s)
}
