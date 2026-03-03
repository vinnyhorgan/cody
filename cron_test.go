package main

import (
	"testing"
	"time"
)

func TestParseScheduleAt(t *testing.T) {
	sched, err := parseSchedule("at 2025-06-15T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if sched.Kind != "at" {
		t.Errorf("kind = %q, want %q", sched.Kind, "at")
	}
}

func TestParseScheduleEvery(t *testing.T) {
	sched, err := parseSchedule("every 30m")
	if err != nil {
		t.Fatal(err)
	}
	if sched.Kind != "every" {
		t.Errorf("kind = %q, want %q", sched.Kind, "every")
	}

	sched, err = parseSchedule("every 2h")
	if err != nil {
		t.Fatal(err)
	}
	if sched.Kind != "every" {
		t.Errorf("kind = %q, want %q", sched.Kind, "every")
	}
}

func TestParseScheduleCron(t *testing.T) {
	sched, err := parseSchedule("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	if sched.Kind != "cron" {
		t.Errorf("kind = %q, want %q", sched.Kind, "cron")
	}
}

func TestParseScheduleInvalid(t *testing.T) {
	invalids := []string{
		"at not-a-date",
		"every not-a-duration",
		"invalid cron expression here",
		"",
	}
	for _, s := range invalids {
		_, err := parseSchedule(s)
		if err == nil {
			t.Errorf("expected error for %q", s)
		}
	}
}

func TestComputeNextRunAt(t *testing.T) {
	sched := CronSchedule{Kind: "at", Raw: "at 2025-12-25T00:00:00Z"}
	next, err := computeNextRun(sched, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := time.Parse(time.RFC3339, "2025-12-25T00:00:00Z")
	if !next.Equal(expected) {
		t.Errorf("next = %v, want %v", next, expected)
	}
}

func TestComputeNextRunEvery(t *testing.T) {
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	sched := CronSchedule{Kind: "every", Raw: "every 1h"}
	next, err := computeNextRun(sched, now)
	if err != nil {
		t.Fatal(err)
	}
	expected := now.Add(time.Hour)
	if !next.Equal(expected) {
		t.Errorf("next = %v, want %v", next, expected)
	}
}

func TestComputeNextRunCron(t *testing.T) {
	now := time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
	sched := CronSchedule{Kind: "cron", Raw: "0 9 * * *"}
	next, err := computeNextRun(sched, now)
	if err != nil {
		t.Fatal(err)
	}
	// Next 9:00 AM after 8:00 AM should be same day 9:00
	if next.Hour() != 9 {
		t.Errorf("next hour = %d, want 9", next.Hour())
	}
	if next.Day() != 1 {
		t.Errorf("next day = %d, want 1", next.Day())
	}
}

func TestComputeNextRunInvalidRaw(t *testing.T) {
	tests := []CronSchedule{
		{Kind: "at", Raw: "at invalid-time"},
		{Kind: "every", Raw: "every not-a-duration"},
	}
	for _, sched := range tests {
		if _, err := computeNextRun(sched, time.Now()); err == nil {
			t.Fatalf("expected error for schedule %+v", sched)
		}
	}
}

func TestCronServiceAddRemoveList(t *testing.T) {
	dir := t.TempDir()
	storePath := dir + "/cron.json"

	svc := newCronService(storePath, nil)
	svc.loadStore()

	// Add job
	job, err := svc.addJob("test-job", "every 1h", "Do something", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "test-job" {
		t.Errorf("job name = %q, want %q", job.Name, "test-job")
	}
	if !job.Enabled {
		t.Error("new job should be enabled")
	}

	// List
	jobs := svc.listJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}

	// Enable/Disable
	svc.enableJob(job.ID, false)
	jobs = svc.listJobs()
	if jobs[0].Enabled {
		t.Error("job should be disabled")
	}

	svc.enableJob(job.ID, true)
	jobs = svc.listJobs()
	if !jobs[0].Enabled {
		t.Error("job should be re-enabled")
	}

	// Remove
	if !svc.removeJob(job.ID) {
		t.Error("remove should return true")
	}
	if svc.removeJob("nonexistent") {
		t.Error("remove nonexistent should return false")
	}

	jobs = svc.listJobs()
	if len(jobs) != 0 {
		t.Errorf("expected 0 jobs after removal, got %d", len(jobs))
	}
}

func TestCronServicePersistence(t *testing.T) {
	dir := t.TempDir()
	storePath := dir + "/cron.json"

	svc1 := newCronService(storePath, nil)
	svc1.loadStore()
	svc1.addJob("persist-test", "every 2h", "Test persistence", true, "", "chat123")

	// Load fresh service
	svc2 := newCronService(storePath, nil)
	svc2.loadStore()

	jobs := svc2.listJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job after reload, got %d", len(jobs))
	}
	if jobs[0].Name != "persist-test" {
		t.Errorf("job name = %q, want %q", jobs[0].Name, "persist-test")
	}
	if !jobs[0].Deliver {
		t.Error("job deliver should be true")
	}
}

func TestCronServiceInvalidSchedule(t *testing.T) {
	dir := t.TempDir()
	svc := newCronService(dir+"/cron.json", nil)
	svc.loadStore()

	_, err := svc.addJob("bad", "invalid schedule", "msg", false, "", "")
	if err == nil {
		t.Error("expected error for invalid schedule")
	}
}

func TestCronTimezone(t *testing.T) {
	sched := CronSchedule{Kind: "cron", Raw: "0 9 * * *", TZ: "America/New_York"}
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	nextRun, err := computeNextRun(sched, now)
	if err != nil {
		t.Fatal(err)
	}
	if nextRun.IsZero() {
		t.Error("next run should not be zero")
	}
	// Should be in UTC (converted back)
	if nextRun.Location() != time.UTC {
		t.Errorf("next run should be UTC, got %v", nextRun.Location())
	}
}

func TestCronServiceAddJobWithTimezone(t *testing.T) {
	dir := t.TempDir()
	svc := newCronService(dir+"/cron.json", nil)
	svc.loadStore()

	job, err := svc.addJob("tz-test", "0 9 * * *", "morning task", false, "Europe/London", "")
	if err != nil {
		t.Fatal(err)
	}
	if job.Schedule.TZ != "Europe/London" {
		t.Errorf("job timezone = %q, want Europe/London", job.Schedule.TZ)
	}
}

func TestCronServiceEnableRecomputesNextRunAndDisableClears(t *testing.T) {
	dir := t.TempDir()
	svc := newCronService(dir+"/cron.json", nil)
	svc.loadStore()

	job, err := svc.addJob("recompute-test", "every 1h", "msg", false, "", "")
	if err != nil {
		t.Fatal(err)
	}

	// Force a stale/past next run to verify enable(true) recomputes from now.
	job.State.NextRunAt = time.Now().Add(-time.Minute)

	if !svc.enableJob(job.ID, true) {
		t.Fatal("enableJob should succeed")
	}

	jobs := svc.listJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if !jobs[0].State.NextRunAt.After(time.Now()) {
		t.Fatalf("expected recomputed future next run, got %v", jobs[0].State.NextRunAt)
	}

	if !svc.enableJob(job.ID, false) {
		t.Fatal("disable should succeed")
	}
	jobs = svc.listJobs()
	if !jobs[0].State.NextRunAt.IsZero() {
		t.Fatalf("expected zero next run when disabled, got %v", jobs[0].State.NextRunAt)
	}
}
