package router_test

import (
	"strings"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/ocr-routerv1/internal/router"
)

// TestUploadAppliesTheDefaultJobTTL is F11 (REPORT 2026-11-10): a customer
// with no TTL of their own got a job with no deadline, so a job nobody could
// run (a deactivated owner, a label whose workers never return) stayed queued
// forever. The router's default fills the gap; a customer's own TTL still
// wins; a zero default restores "no deadline".
func TestUploadAppliesTheDefaultJobTTL(t *testing.T) {
	cases := []struct {
		name       string
		defaultTTL time.Duration
		userSecs   int
		want       time.Time
	}{
		{"default fills an unset TTL", time.Hour, 0, base.Add(time.Hour)},
		{"the customer's own TTL wins", time.Hour, 60, base.Add(time.Minute)},
		{"a zero default means no deadline", 0, 0, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarnessWith(t, router.Config{
				Lease: 5 * time.Minute, MaxAttempts: 3, MaxReclaims: 10,
				AgingStep: time.Minute, LabelGrace: 5 * time.Minute,
				ResultTTL: time.Hour, DefaultLabel: "ocr",
				DefaultJobTTL: c.defaultTTL,
			})
			h.worker(t, "ocr")
			h.user(t, "u", 100, 4, c.userSecs)
			j := upload(t, h, "u", router.UploadInput{Body: strings.NewReader("x")})
			if !j.ExpiresAt.Equal(c.want) {
				t.Errorf("ExpiresAt = %v, want %v", j.ExpiresAt, c.want)
			}
		})
	}
}
