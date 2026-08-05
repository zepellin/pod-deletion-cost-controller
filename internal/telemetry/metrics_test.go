package telemetry_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zepellin/pod-deletion-cost-controller/internal/telemetry"
)

func TestCheckLiveness_StandbyReplicaIsAlwaysLive(t *testing.T) {
	r := telemetry.NewRecorder()
	// A replica that never acquired the lease is not expected to sync at all,
	// so even a very short staleness limit must not fail it.
	if err := r.CheckLiveness(time.Nanosecond); err != nil {
		t.Errorf("standby replica: want live, got %v", err)
	}
}

func TestCheckLiveness_LeaderAfterSync(t *testing.T) {
	r := telemetry.NewRecorder()
	r.SetLeading(true)
	r.ObserveSyncCycle(10*time.Millisecond, false)

	if err := r.CheckLiveness(time.Minute); err != nil {
		t.Errorf("fresh leader: want live, got %v", err)
	}
}

func TestCheckLiveness_LeaderWithStaleSync(t *testing.T) {
	r := telemetry.NewRecorder()
	r.SetLeading(true)
	r.ObserveSyncCycle(10*time.Millisecond, false)

	time.Sleep(10 * time.Millisecond)

	err := r.CheckLiveness(time.Millisecond)
	if err == nil {
		t.Fatal("stale leader: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no sync cycle completed") {
		t.Errorf("unexpected error text: %v", err)
	}
}

func TestCheckLiveness_LosingLeadershipClearsStaleness(t *testing.T) {
	r := telemetry.NewRecorder()
	r.SetLeading(true)
	time.Sleep(2 * time.Millisecond)
	if err := r.CheckLiveness(time.Millisecond); err == nil {
		t.Fatal("stale leader: want error, got nil")
	}

	// Stepping down must make the replica healthy again — it is no longer
	// responsible for syncing.
	r.SetLeading(false)
	if err := r.CheckLiveness(time.Millisecond); err != nil {
		t.Errorf("after stepping down: want live, got %v", err)
	}
}

func TestHandler_ExposesControllerMetrics(t *testing.T) {
	r := telemetry.NewRecorder()
	r.SetLeading(true)
	r.ObserveSyncCycle(time.Second, false)
	r.SetPodsManaged("default", "busy", 3)
	r.RecordAnnotationPatch("default", "updated")
	r.RecordMetricsUnavailable("default", "not_found")

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("metrics endpoint: got status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"pdcc_sync_cycles_total",
		"pdcc_sync_duration_seconds",
		"pdcc_pods_managed",
		"pdcc_annotation_patches_total",
		"pdcc_metrics_unavailable_total",
		"pdcc_leader 1",
		"pdcc_last_sync_timestamp_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}
