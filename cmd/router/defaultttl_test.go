package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"
)

// TestDefaultJobTTLReachesTheRouter: the flag's value is the deadline a job of
// a customer with no TTL gets, through the binary's own graph (F11). Every
// router test builds router.Config by hand, so only this one sees buildApp
// drop the field.
func TestDefaultJobTTLReachesTheRouter(t *testing.T) {
	cfg := testConfig(t)
	cfg.DefaultJobTTL = 3 * time.Hour
	app, srv := startApp(t, cfg)
	clientTok, workerTok, _ := seedAccounts(t, app)

	workerEvents, cancelWorker := openStream(t, srv.URL+"/sse?label=ocr", workerTok)
	defer cancelWorker()
	awaitEvent(t, workerEvents, "hello", 3*time.Second)

	before := time.Now()
	c := client{t: t, base: srv.URL, tok: clientTok}
	resp := c.req("POST", "/upload?label=ocr&url=x", nil, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201", resp.StatusCode)
	}
	var created struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	job, err := app.Repo.JobByID(context.Background(), created.JobID)
	if err != nil {
		t.Fatalf("JobByID: %v", err)
	}
	lo, hi := before.Add(3*time.Hour-time.Minute), time.Now().Add(3*time.Hour+time.Minute)
	if job.ExpiresAt.Before(lo) || job.ExpiresAt.After(hi) {
		t.Errorf("ExpiresAt = %v, want about upload + 3h — --default-job-ttl did not reach the router", job.ExpiresAt)
	}
}

// TestDefaultJobTTLFlagDefaultsToAWeek pins the documented default.
func TestDefaultJobTTLFlagDefaultsToAWeek(t *testing.T) {
	cmd := newCLI()
	var got Config
	cmd.Action = func(_ context.Context, c *cli.Command) error {
		got = configFrom(c)
		return nil
	}
	if err := cmd.Run(context.Background(), []string{"router"}); err != nil {
		t.Fatalf("running with no flags: %v", err)
	}
	if got.DefaultJobTTL != 7*24*time.Hour {
		t.Errorf("--default-job-ttl defaults to %v, want 168h as the README says", got.DefaultJobTTL)
	}
}

// TestNegativeDefaultJobTTLIsRefused: a negative value would silently mean
// "no deadline"; the boot refuses it and names the flag.
func TestNegativeDefaultJobTTLIsRefused(t *testing.T) {
	cfg := testConfig(t)
	cfg.DefaultJobTTL = -time.Hour
	app, err := buildApp(cfg)
	if err == nil {
		_ = app.Close()
		t.Fatal("buildApp accepted --default-job-ttl -1h")
	}
	if !strings.Contains(err.Error(), "default-job-ttl") {
		t.Errorf("error %q does not name --default-job-ttl", err)
	}
}
