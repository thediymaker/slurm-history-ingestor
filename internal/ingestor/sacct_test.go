package ingestor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thediymaker/slurm-history-ingestor/internal/config"
)

func TestParseGpuCount(t *testing.T) {
	tests := []struct {
		name string
		tres string
		want int32
	}{
		{"empty", "", 0},
		{"no gpu", "billing=8,cpu=8,mem=64G,node=1", 0},
		{"plain gres/gpu", "billing=8,cpu=8,gres/gpu=4,mem=64G,node=1", 4},
		{"gres/gpu first", "gres/gpu=2,cpu=8", 2},
		{"typed gpu with colon", "cpu=8,gres/gpu:a100=3,mem=64G", 3},
		{"zero gpus", "cpu=8,gres/gpu=0", 0},
		{"whitespace only", "   ", 0},
		{"gpu not a number", "gres/gpu=abc", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGpuCount(tt.tres); got != tt.want {
				t.Errorf("parseGpuCount(%q) = %d, want %d", tt.tres, got, tt.want)
			}
		})
	}
}

func TestParseMemory(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"empty", "", 0},
		{"kilobytes", "2048K", 2048 * 1024},
		{"megabytes", "512M", 512 * 1024 * 1024},
		{"gigabytes", "2G", 2 * 1024 * 1024 * 1024},
		{"fractional megabytes", "1.5M", int64(1.5 * 1024 * 1024)},
		{"no suffix is bytes", "1024", 1024},
		{"surrounding whitespace", "  4K  ", 4 * 1024},
		{"unparseable", "abc", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseMemory(tt.in); got != tt.want {
				t.Errorf("parseMemory(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseInt32(t *testing.T) {
	tests := []struct {
		in   string
		want int32
	}{
		{"", 0},
		{"8", 8},
		{"0", 0},
		{"abc", 0},
		{"-3", -3},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseInt32(tt.in); got != tt.want {
				t.Errorf("parseInt32(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSlurmTime(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantZero bool
		want     time.Time
	}{
		{"empty", "", true, time.Time{}},
		{"unknown sentinel", "Unknown", true, time.Time{}},
		{"none sentinel", "None", true, time.Time{}},
		{"malformed", "not-a-date", true, time.Time{}},
		{"iso T separator", "2024-01-15T10:30:45", false, time.Date(2024, 1, 15, 10, 30, 45, 0, time.UTC)},
		{"iso space separator", "2024-01-15 10:30:45", false, time.Date(2024, 1, 15, 10, 30, 45, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSlurmTime(tt.in)
			if tt.wantZero {
				if !got.IsZero() {
					t.Errorf("parseSlurmTime(%q) = %v, want zero", tt.in, got)
				}
				return
			}
			if !got.Equal(tt.want) {
				t.Errorf("parseSlurmTime(%q) = %v, want %v", tt.in, got, tt.want)
			}
			// Must be interpreted as UTC, not the local zone.
			if got.Location() != time.UTC {
				t.Errorf("parseSlurmTime(%q) location = %v, want UTC", tt.in, got.Location())
			}
		})
	}
}

func TestAppendTZ(t *testing.T) {
	t.Run("strips existing TZ and appends", func(t *testing.T) {
		env := []string{"PATH=/usr/bin", "TZ=America/Phoenix", "HOME=/home/x"}
		out := appendTZ(env, "UTC")

		var tzCount int
		for _, kv := range out {
			if strings.HasPrefix(kv, "TZ=") {
				tzCount++
			}
		}
		if tzCount != 1 {
			t.Fatalf("expected exactly one TZ entry, got %d in %v", tzCount, out)
		}
		// glibc getenv returns the first match, so our value must be the sole one
		// and no stale America/Phoenix may remain.
		if last := out[len(out)-1]; last != "TZ=UTC" {
			t.Errorf("expected TZ=UTC appended last, got %q", last)
		}
		for _, kv := range out {
			if kv == "TZ=America/Phoenix" {
				t.Errorf("stale TZ entry not stripped: %v", out)
			}
		}
	})

	t.Run("appends when no TZ present", func(t *testing.T) {
		env := []string{"PATH=/usr/bin"}
		out := appendTZ(env, "UTC")
		if len(out) != 2 || out[len(out)-1] != "TZ=UTC" {
			t.Errorf("appendTZ = %v, want PATH plus TZ=UTC", out)
		}
	})
}

// buildSacctLine assembles a pipe-delimited sacct line from the 19 fields in
// sacctFormat order, so tests stay readable as the format evolves.
func buildSacctLine(fields ...string) string {
	return strings.Join(fields, "|")
}

func TestParseSacctLine(t *testing.T) {
	s := &SacctIngestor{}

	validFields := []string{
		"12345",                    // 0 JobIDRaw
		"alice",                    // 1 User
		"physics",                  // 2 Account
		"gpu",                      // 3 Partition
		"COMPLETED",                // 4 State
		"0:0",                      // 5 ExitCode
		"2024-01-15T10:00:00",      // 6 Submit
		"2024-01-15T10:05:00",      // 7 Start
		"2024-01-15T11:05:00",      // 8 End
		"8",                        // 9 AllocCPUS
		"1",                        // 10 AllocNodes
		"node01",                   // 11 NodeList
		"myjob",                    // 12 JobName
		"2048K",                    // 13 MaxRSS
		"60",                       // 14 TimelimitRaw
		"normal",                   // 15 QOS
		"physicsgrp",               // 16 Group
		"cpu=8,gres/gpu=4,mem=64G", // 17 AllocTRES
		"1-02:03:04.600",           // 18 TotalCPU
	}

	t.Run("happy path", func(t *testing.T) {
		job, err := s.parseSacctLine(buildSacctLine(validFields...))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if job.JobID != 12345 {
			t.Errorf("JobID = %d, want 12345", job.JobID)
		}
		if job.User != "alice" || job.Account != "physics" {
			t.Errorf("User/Account = %q/%q, want alice/physics", job.User, job.Account)
		}
		if job.ExitCode != 0 {
			t.Errorf("ExitCode = %d, want 0", job.ExitCode)
		}
		if job.AllocCPUs != 8 || job.AllocNodes != 1 {
			t.Errorf("AllocCPUs/Nodes = %d/%d, want 8/1", job.AllocCPUs, job.AllocNodes)
		}
		if job.MaxRSS != 2048*1024 {
			t.Errorf("MaxRSS = %d, want %d", job.MaxRSS, 2048*1024)
		}
		if job.Timelimit != 60 {
			t.Errorf("Timelimit = %d, want 60", job.Timelimit)
		}
		if job.GpuCount != 4 {
			t.Errorf("GpuCount = %d, want 4", job.GpuCount)
		}
		if job.AllocTRES != "cpu=8,gres/gpu=4,mem=64G" {
			t.Errorf("AllocTRES = %q", job.AllocTRES)
		}
		if want := int64(86400 + 2*3600 + 3*60 + 5); job.TotalCPU == nil || *job.TotalCPU != want {
			t.Errorf("TotalCPU = %v, want %d", job.TotalCPU, want)
		}
		wantStart := time.Date(2024, 1, 15, 10, 5, 0, 0, time.UTC)
		if !job.StartTime.Equal(wantStart) {
			t.Errorf("StartTime = %v, want %v", job.StartTime, wantStart)
		}
	})

	t.Run("array job strips suffix", func(t *testing.T) {
		f := append([]string(nil), validFields...)
		f[0] = "12345_7"
		job, err := s.parseSacctLine(buildSacctLine(f...))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if job.JobID != 12345 {
			t.Errorf("JobID = %d, want 12345 (array suffix stripped)", job.JobID)
		}
	})

	t.Run("batch step strips dot suffix", func(t *testing.T) {
		f := append([]string(nil), validFields...)
		f[0] = "12345.batch"
		job, err := s.parseSacctLine(buildSacctLine(f...))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if job.JobID != 12345 {
			t.Errorf("JobID = %d, want 12345 (.batch suffix stripped)", job.JobID)
		}
	})

	t.Run("exit code with signal", func(t *testing.T) {
		f := append([]string(nil), validFields...)
		f[5] = "9:15"
		job, err := s.parseSacctLine(buildSacctLine(f...))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if job.ExitCode != 9 {
			t.Errorf("ExitCode = %d, want 9", job.ExitCode)
		}
	})

	t.Run("unlimited timelimit is zero", func(t *testing.T) {
		f := append([]string(nil), validFields...)
		f[14] = "UNLIMITED"
		job, err := s.parseSacctLine(buildSacctLine(f...))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if job.Timelimit != 0 {
			t.Errorf("Timelimit = %d, want 0 for UNLIMITED", job.Timelimit)
		}
	})

	t.Run("empty TotalCPU is nil", func(t *testing.T) {
		f := append([]string(nil), validFields...)
		f[18] = ""
		job, err := s.parseSacctLine(buildSacctLine(f...))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if job.TotalCPU != nil {
			t.Errorf("TotalCPU = %d, want nil", *job.TotalCPU)
		}
	})

	t.Run("too few fields errors", func(t *testing.T) {
		if _, err := s.parseSacctLine("12345|alice|physics"); err == nil {
			t.Error("expected error for line with fewer than 19 fields")
		}
		// The pre-TotalCPU format had 18 fields
		if _, err := s.parseSacctLine(buildSacctLine(validFields[:18]...)); err == nil {
			t.Error("expected error for line without TotalCPU")
		}
	})

	t.Run("invalid job id errors", func(t *testing.T) {
		f := append([]string(nil), validFields...)
		f[0] = "notanumber"
		if _, err := s.parseSacctLine(buildSacctLine(f...)); err == nil {
			t.Error("expected error for non-numeric job ID")
		}
	})
}

func TestParseSlurmDuration(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"", 0, false},
		{"00:00:00", 0, true},
		{"05:06.789", 5*60 + 7, true}, // MM:SS.mmm, rounded
		{"00:30.400", 30, true},
		{"01:02:03", 3600 + 2*60 + 3, true},
		{"1-02:03:04", 86400 + 2*3600 + 3*60 + 4, true},
		{"12-00:00:00", 12 * 86400, true},
		{"  00:01:00  ", 60, true},
		{"42", 0, false},
		{"1:2:3:4", 0, false},
		{"x-01:00:00", 0, false},
		{"aa:bb", 0, false},
		{"INVALID", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseSlurmDuration(tt.in)
			if !tt.ok {
				if got != nil {
					t.Errorf("parseSlurmDuration(%q) = %d, want nil", tt.in, *got)
				}
				return
			}
			if got == nil || *got != tt.want {
				t.Errorf("parseSlurmDuration(%q) = %v, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSacctStep(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		wantParent string
		wantRSS    int64
		wantOK     bool
	}{
		{"job line is not a step", "12345|alice|physics", "", 0, false},
		{"batch step", "12345.batch||||COMPLETED|0:0|||||||batch|2048K|||||00:10.000", "12345", 2048 * 1024, true},
		{"numbered step", "12345.0||||COMPLETED|0:0|||||||python|3G|||||01:00:00", "12345", 3 * 1024 * 1024 * 1024, true},
		{"short step line", "12345.extern", "12345", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent, rss, ok := parseSacctStep(tt.line)
			if ok != tt.wantOK || parent != tt.wantParent || rss != tt.wantRSS {
				t.Errorf("parseSacctStep(%q) = %q, %d, %v; want %q, %d, %v",
					tt.line, parent, rss, ok, tt.wantParent, tt.wantRSS, tt.wantOK)
			}
		})
	}
}

// TestFetchJobsFoldsSteps runs fetchJobs against a stand-in sacct that prints
// two jobs with their steps, and checks that steps aren't returned as jobs and
// that each job gets its steps' highest MaxRSS.
func TestFetchJobsFoldsSteps(t *testing.T) {
	out := strings.Join([]string{
		"100|alice|physics|cpu|COMPLETED|0:0|2024-01-15T10:00:00|2024-01-15T10:05:00|2024-01-15T11:05:00|8|1|node01|job-a||60|normal|grp|cpu=8,mem=16G,node=1|02:00:00",
		"100.batch||||COMPLETED|0:0|||||||batch|1G|||||00:30:00",
		"100.extern||||COMPLETED|0:0|||||||extern|0|||||00:00:00",
		"100.0||||COMPLETED|0:0|||||||python|5G|||||01:30:00",
		"101|bob|chem|cpu|FAILED|1:0|2024-01-15T10:00:00|2024-01-15T10:01:00|2024-01-15T10:02:00|1|1|node02|job-b||10|normal|grp|cpu=1,node=1|",
		"101.batch||||FAILED|1:0|||||||batch|300M|||||00:00:01.200",
	}, "\n")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte(out+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "sacct")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat \""+filepath.Join(dir, "out.txt")+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &SacctIngestor{cfg: &config.Config{SacctPath: script, ClusterName: "test"}}
	start := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	jobs, err := s.fetchJobs(context.Background(), start, start.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("fetchJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2 (steps must not become jobs)", len(jobs))
	}
	if jobs[0].JobID != 100 || jobs[0].MaxRSS != 5*1024*1024*1024 {
		t.Errorf("job 100: id %d MaxRSS %d, want the largest step MaxRSS (5G)", jobs[0].JobID, jobs[0].MaxRSS)
	}
	if jobs[0].TotalCPU == nil || *jobs[0].TotalCPU != 7200 {
		t.Errorf("job 100: TotalCPU %v, want 7200 from the job line", jobs[0].TotalCPU)
	}
	if jobs[1].MaxRSS != 300*1024*1024 {
		t.Errorf("job 101: MaxRSS %d, want 300M", jobs[1].MaxRSS)
	}
	if jobs[1].TotalCPU != nil {
		t.Errorf("job 101: TotalCPU %d, want nil for an empty field", *jobs[1].TotalCPU)
	}
}
