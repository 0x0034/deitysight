package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// WindowSpec describes evidence selection, not an analysis or ranking request.
type WindowSpec struct {
	Window, Step   time.Duration
	Scenes         []string
	IncludeThreads bool
}

var allScenes = []string{"cpu", "io", "mem", "network"}
var sceneLabels = map[string][]string{
	"cpu":     {"CPU", "cpu", "CPL", "PRC", "PSI"},
	"io":      {"DSK", "LVM", "MDD", "PRD", "PSI"},
	"mem":     {"MEM", "SWP", "PAG", "PRM", "PSI"},
	"network": {"NET", "PRN"},
}

func normalizeScenes(in []string) ([]string, error) {
	if in == nil {
		return slices.Clone(allScenes), nil
	}
	if len(in) == 0 || len(in) > len(allScenes) {
		return nil, errors.New("invalid scenes")
	}
	seen := map[string]bool{}
	for _, s := range in {
		if _, ok := sceneLabels[s]; !ok || seen[s] {
			return nil, errors.New("invalid scenes")
		}
		seen[s] = true
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return out, nil
}
func labelsFor(scenes []string) []string {
	if len(scenes) == 0 {
		scenes = allScenes
	}
	set := map[string]bool{"PRG": true}
	for _, scene := range scenes {
		for _, label := range sceneLabels[scene] {
			set[label] = true
		}
	}
	// Match atop's fixed output order; users never supply executable arguments.
	var out []string
	for _, l := range []string{"CPU", "cpu", "CPL", "MEM", "SWP", "PAG", "PSI", "LVM", "MDD", "DSK", "NET", "PRG", "PRC", "PRM", "PRD", "PRN"} {
		if set[l] {
			out = append(out, l)
		}
	}
	return out
}

var errAtopFormat = errors.New("invalid or incomplete atop 2.7.1 stream")
var processName = regexp.MustCompile(`^\((.{0,15})\) ([A-Za-z]) (.*)$`)

// ParseAtopStream retains at most one bounded line. A source line is usable only
// if its sample_id has a subsequent frame_end (SEP) commit record. Command lines
// and malformed input never reach the sink, including diagnostics.
func ParseAtopStream(input io.Reader, spec WindowSpec, limit int, emit func(Record) error) error {
	if limit < 256 || limit > 16<<20 {
		return errAtopFormat
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, min(limit, 64<<10)), limit)
	selected := map[string]bool{}
	for _, l := range labelsFor(spec.Scenes) {
		selected[l] = true
	}
	id := ""
	baseline := false
	first := true
	epoch := int64(0)
	interval := int64(0)
	host := ""
	records := 0
	seen := map[string]bool{}
	for scanner.Scan() {
		line := scanner.Text()
		if !utf8.ValidString(line) || strings.IndexByte(line, 0) >= 0 {
			return errAtopFormat
		}
		if line == "RESET" {
			if id != "" || !first {
				return errAtopFormat
			}
			baseline = true
			continue
		}
		if line == "SEP" {
			if id == "" || records == 0 || !completeLabels(spec.Scenes, seen) {
				return errAtopFormat
			}
			r := Record{SchemaVersion: 2, SampleID: id, Kind: "frame_end", Source: "atop/SEP", Complete: true, Baseline: baseline, Epoch: epoch, IntervalSeconds: interval, Hostname: host, StartedAt: time.Unix(epoch, 0).UTC(), FinishedAt: time.Now().UTC()}
			if e := emit(r); e != nil {
				return e
			}
			id = ""
			records = 0
			seen = map[string]bool{}
			baseline = false
			first = false
			continue
		}
		fields := strings.SplitN(line, " ", 7)
		if len(fields) != 7 || !selected[fields[0]] {
			return errAtopFormat
		}
		e, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || e <= 0 {
			return errAtopFormat
		}
		dt, err := strconv.ParseInt(fields[5], 10, 64)
		if err != nil || dt <= 0 {
			return errAtopFormat
		}
		if id == "" {
			if first && !baseline {
				return errAtopFormat
			}
			if !first && e <= epoch {
				return errAtopFormat
			}
			id = uuid()
			epoch = e
			interval = dt
			host = fields[1]
		}
		if e != epoch || dt != interval || fields[1] != host {
			return errAtopFormat
		}
		r := Record{SchemaVersion: 2, SampleID: id, Kind: "source", Source: "atop/" + fields[0], Scope: "host", Content: line + "\n", Encoding: "utf8", Complete: true, Baseline: baseline, Epoch: e, IntervalSeconds: dt, Hostname: host, StartedAt: time.Unix(e, 0).UTC(), FinishedAt: time.Now().UTC()}
		if strings.HasPrefix(fields[0], "PR") {
			pidText, body, ok := strings.Cut(fields[6], " ")
			if !ok {
				return errAtopFormat
			}
			pid, err := strconv.Atoi(pidText)
			if err != nil || pid <= 0 {
				return errAtopFormat
			}
			m := processName.FindStringSubmatch(body)
			if m == nil {
				return errAtopFormat
			}
			tail := m[2] + " " + m[3]
			v := strings.Fields(tail)
			isproc := ""
			tgid := ""
			obj := &Object{PID: pid}
			if fields[0] == "PRG" {
				// The first seven fields and last sixteen fields surround the only command
				// line. Split by the final delimiter, so nested command parentheses work.
				prefix := strings.SplitN(tail, " ", 8)
				if len(prefix) != 8 {
					return errAtopFormat
				}
				cmdAndSuffix := prefix[7]
				end := strings.LastIndex(cmdAndSuffix, ") ")
				if !strings.HasPrefix(cmdAndSuffix, "(") || end < 0 {
					return errAtopFormat
				}
				suffix := strings.Fields(cmdAndSuffix[end+2:])
				if len(suffix) != 16 {
					return errAtopFormat
				}
				for _, n := range append(slices.Clone(prefix[1:7]), suffix[:11]...) {
					if _, err := strconv.ParseInt(n, 10, 64); err != nil {
						return errAtopFormat
					}
				}
				isproc = suffix[11]
				tgid = prefix[3]
				obj.StartEpoch = prefix[6]
				if suffix[14] != "-" {
					obj.ContainerID = suffix[14]
				}
				r.Content = strings.Join(fields[:6], " ") + " " + pidText + " (" + m[1] + ") " + strings.Join(prefix[:7], " ") + " () " + strings.Join(suffix, " ") + "\n"
				r.RedactedFields = []string{"command_line"}
			} else {
				switch fields[0] {
				case "PRC":
					if len(v) != 14 {
						return errAtopFormat
					}
					tgid = v[10]
					isproc = v[11]
				case "PRM":
					if len(v) != 17 {
						return errAtopFormat
					}
					tgid = v[13]
					isproc = v[14]
				case "PRD":
					if len(v) != 11 {
						return errAtopFormat
					}
					tgid = v[8]
					isproc = v[10]
					r.Supported = boolFlag(v[2])
				case "PRN":
					if len(v) != 14 {
						return errAtopFormat
					}
					tgid = v[12]
					isproc = v[13]
					r.Supported = boolFlag(v[1])
				default:
					return errAtopFormat
				}
			}
			if isproc != "y" && isproc != "n" {
				return errAtopFormat
			}
			group, err := strconv.Atoi(tgid)
			if err != nil || group <= 0 {
				return errAtopFormat
			}
			r.Scope = "process"
			r.Object = obj
			if isproc == "n" {
				if !spec.IncludeThreads {
					continue
				}
				r.Scope = "thread"
				obj.PID = group
				obj.TID = pid
			}
		}
		if fields[0] == "PSI" {
			v := strings.Fields(fields[6])
			if len(v) == 0 {
				return errAtopFormat
			}
			r.Supported = boolFlag(v[0])
			if r.Supported == nil {
				return errAtopFormat
			}
		}
		seen[fields[0]] = true
		records++
		if err := emit(r); err != nil {
			return err
		}
	}
	if scanner.Err() != nil {
		return errAtopFormat
	}
	if id != "" || first || baseline {
		return errAtopFormat
	}
	return nil
}
func boolFlag(s string) *bool {
	if s != "y" && s != "n" {
		return nil
	}
	v := s == "y"
	return &v
}

type windowCollector interface {
	CollectWindow(context.Context, WindowSpec, func(Record) error) error
	Available() error
}

func (s WindowSpec) args() []string {
	count := int64((s.Window+s.Step-1)/s.Step) + 1
	return []string{"-P", strings.Join(labelsFor(s.Scenes), ","), strconv.FormatInt(int64(s.Step/time.Second), 10), strconv.FormatInt(count, 10)}
}
func scenarioError(err error) string {
	switch {
	case errors.Is(err, errStorage):
		return "storage_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "atop_timeout"
	case errors.Is(err, context.Canceled):
		return "atop_cancelled"
	case errors.Is(err, errAtopFormat):
		return "atop_invalid_stream"
	default:
		return "atop_failed"
	}
}
func staticLimitations() []string {
	return []string{"agent_does_not_compute_metrics", "command_line_removed", "no_process_accounting_short_lived_processes_may_be_missing", "io_await_and_queue_unavailable_in_atop_2.7.1", "process_io_is_not_per_device", "network_process_counters_require_compatible_netatop", "pss_not_collected", "host_boot_id_not_collected", "thread_filter_does_not_reduce_atop_internal_scan"}
}

// LVM/MDD/DSK may legitimately have no rows on a host with no corresponding
// devices. Their absence is not proof of zero activity. PSI/PRD/PRN still emit
// explicit support flags even when their optional kernel facilities are absent.
func completeLabels(scenes []string, seen map[string]bool) bool {
	if len(scenes) == 0 {
		scenes = allScenes
	}
	if !seen["PRG"] {
		return false
	}
	required := map[string][]string{
		"cpu":     {"CPU", "cpu", "CPL", "PRC", "PSI"},
		"io":      {"PRD", "PSI"},
		"mem":     {"MEM", "SWP", "PAG", "PRM", "PSI"},
		"network": {"NET", "PRN"},
	}
	for _, s := range scenes {
		for _, l := range required[s] {
			if !seen[l] {
				return false
			}
		}
	}
	return true
}
