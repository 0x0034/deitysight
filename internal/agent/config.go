package agent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	HTTP       HTTPConfig       `yaml:"http"`
	Storage    StorageConfig    `yaml:"storage"`
	Background BackgroundConfig `yaml:"background"`
	Sampling   SamplingConfig   `yaml:"sampling"`
	Atop       AtopConfig       `yaml:"atop"`
}
type HTTPConfig struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}
type StorageConfig struct {
	Path            string        `yaml:"path"`
	ResultRetention time.Duration `yaml:"result_retention"`
	TaskRetention   time.Duration `yaml:"task_retention"`
	MaxBytes        int64         `yaml:"max_bytes"`
	MinFreeBytes    uint64        `yaml:"min_free_bytes"`
}
type BackgroundConfig struct {
	Enabled   bool          `yaml:"enabled"`
	Step      time.Duration `yaml:"step"`
	Retention time.Duration `yaml:"retention"`
}
type SamplingConfig struct {
	DefaultWindow  time.Duration `yaml:"default_window"`
	DefaultStep    time.Duration `yaml:"default_step"`
	MaxWindow      time.Duration `yaml:"max_window"`
	MinStep        time.Duration `yaml:"min_step"`
	MaxPoints      int           `yaml:"max_points"`
	RoundTimeout   time.Duration `yaml:"round_timeout"`
	MaxSourceBytes int64         `yaml:"max_source_bytes"`
}

// AtopConfig controls the pinned parseable collector. Path and Interval are
// accepted only for legacy configuration migration; raw collection is unavailable.
type AtopConfig struct {
	StartupGrace time.Duration `yaml:"startup_grace"`
	FinishGrace  time.Duration `yaml:"finish_grace"`
	Enabled      bool          `yaml:"enabled"`
	Mode         string        `yaml:"mode"`
	Binary       string        `yaml:"binary"`
	Path         string        `yaml:"path"`
	Interval     time.Duration `yaml:"interval"`
}

func DefaultConfig() Config {
	return Config{
		HTTP:       HTTPConfig{Listen: "127.0.0.1:19100"},
		Storage:    StorageConfig{Path: "/var/lib/deitysight", ResultRetention: 24 * time.Hour, TaskRetention: 7 * 24 * time.Hour, MaxBytes: 1 << 30, MinFreeBytes: 1 << 30},
		Background: BackgroundConfig{Step: 30 * time.Second, Retention: 10 * time.Minute},
		Sampling:   SamplingConfig{DefaultWindow: 30 * time.Second, DefaultStep: 5 * time.Second, MaxWindow: 300 * time.Second, MinStep: time.Second, MaxPoints: 301, RoundTimeout: 2 * time.Second, MaxSourceBytes: 1 << 20},
		Atop:       AtopConfig{Enabled: true, StartupGrace: 10 * time.Second, FinishGrace: 5 * time.Second, Mode: "parseable", Binary: "/usr/bin/atop", Interval: time.Second},
	}
}
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, errors.New("invalid YAML configuration")
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, errors.New("configuration must contain one document")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.HTTP.Token == "" || strings.Contains(c.HTTP.Token, "REPLACE_WITH") || strings.TrimSpace(c.HTTP.Token) != c.HTTP.Token || strings.ContainsAny(c.HTTP.Token, "\r\n") {
		return errors.New("configure a nonempty deployment token")
	}
	if _, _, err := net.SplitHostPort(c.HTTP.Listen); err != nil {
		return errors.New("http.listen must be host:port")
	}
	if !filepath.IsAbs(c.Storage.Path) || filepath.Clean(c.Storage.Path) != c.Storage.Path {
		return errors.New("storage.path must be a clean absolute path")
	}
	for _, p := range []string{"/", "/tmp", "/var", "/var/lib", "/home", "/root", "/usr", "/etc", "/proc", "/sys", "/dev"} {
		if c.Storage.Path == p {
			return errors.New("storage.path must be a dedicated directory")
		}
	}
	for _, p := range []string{"/proc/", "/sys/", "/dev/", "/etc/"} {
		if strings.HasPrefix(c.Storage.Path, p) {
			return errors.New("storage.path is inside a system control directory")
		}
	}
	if c.Storage.ResultRetention <= 0 || c.Storage.TaskRetention < c.Storage.ResultRetention || c.Storage.MaxBytes <= 0 || c.Storage.MinFreeBytes == 0 {
		return errors.New("invalid storage budget or retention")
	}
	if c.Background.Step <= 0 || c.Background.Retention <= 0 {
		return errors.New("background timing must be positive")
	}
	if c.Atop.Enabled && c.Background.Enabled && (c.Background.Step%time.Second != 0 || c.Background.Retention%time.Second != 0 || c.Background.Step > c.Background.Retention) {
		return errors.New("atop background requires whole seconds and step <= retention")
	}
	if c.Atop.StartupGrace < 0 || c.Atop.StartupGrace > time.Minute || c.Atop.FinishGrace < 0 || c.Atop.FinishGrace > time.Minute {
		return errors.New("invalid atop grace")
	}
	if c.Atop.Enabled {
		if !allowedAtopBinary(c.Atop.Binary) || (c.Atop.Mode != "parseable" && c.Atop.Mode != "raw") || c.Atop.Interval <= 0 || c.Atop.Interval > 60*time.Second || c.Atop.Interval%time.Second != 0 {
			return errors.New("invalid atop configuration")
		}
		if c.Atop.Mode == "raw" {
			p := c.Atop.Path
			if p == "" {
				p = c.Storage.Path
			}
			if !validAtopRawPath(c.Storage.Path, p) {
				return errors.New("atop.path must be inside storage.path")
			}
		}
	}
	s := c.Sampling
	if s.MinStep <= 0 || s.MaxWindow <= 0 || s.MaxPoints < 2 || s.MaxPoints > 1000000 || s.RoundTimeout <= 0 || s.MaxSourceBytes < 256 || s.MaxSourceBytes > 16<<20 {
		return errors.New("invalid sampling limits (source line limit must be 256 bytes to 16 MiB)")
	}
	return c.validateSampling(s.DefaultWindow, s.DefaultStep)
}

func allowedAtopBinary(binary string) bool {
	switch filepath.Clean(binary) {
	case "/bin/atop", "/usr/bin/atop", "/usr/sbin/atop", "/usr/local/bin/atop":
		return binary == filepath.Clean(binary)
	default:
		return false
	}
}

func validAtopRawPath(storagePath, rawPath string) bool {
	if !filepath.IsAbs(rawPath) || filepath.Clean(rawPath) != rawPath {
		return false
	}
	return rawPath == storagePath || strings.HasPrefix(rawPath, storagePath+string(filepath.Separator))
}
func (c Config) validateSampling(window, step time.Duration) error {
	if window <= 0 || step <= 0 || window > c.Sampling.MaxWindow || step < c.Sampling.MinStep || step > window || window%time.Second != 0 || step%time.Second != 0 {
		return errors.New("invalid sampling window or step")
	}
	n := window/step + 1
	if window%step != 0 {
		n++
	}
	if n > time.Duration(c.Sampling.MaxPoints) {
		return fmt.Errorf("too many sampling points (maximum %d)", c.Sampling.MaxPoints)
	}
	return nil
}
func Schedule(window, step time.Duration) []time.Duration {
	if step <= 0 || window <= 0 {
		return nil
	}
	points := []time.Duration{0}
	for p := step; p < window; {
		points = append(points, p)
		if window-p <= step {
			break
		}
		p += step
	}
	return append(points, window)
}
