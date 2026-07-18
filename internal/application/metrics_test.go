package application

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"postra/internal/domain"
	"postra/internal/platform/metrics"
)

// TestSyncMetrics verifies POP3 ingest is instrumented and the exposition
// endpoint serves the series. Counters are process-global, so the test asserts
// deltas around its own sync rather than absolute values (§18.1).
func TestSyncMetrics(t *testing.T) {
	app, pop, _, _ := newTestApp(t)
	acc := mustAccount(t, app)
	pop.messages["u1"] = testMail("m1", "first", "body one")
	pop.messages["u2"] = testMail("m2", "second", "body two")

	beforeMsgs := testutil.ToFloat64(metrics.MessagesFetched)
	beforeOK := testutil.ToFloat64(metrics.SyncTotal.WithLabelValues(string(domain.JobSucceeded)))

	job := syncAndWait(t, app, acc.ID)
	if job.Status != domain.JobSucceeded {
		t.Fatalf("sync status = %s, want succeeded", job.Status)
	}

	if got := testutil.ToFloat64(metrics.MessagesFetched) - beforeMsgs; got < 2 {
		t.Fatalf("messages_fetched delta = %v, want >= 2", got)
	}
	if got := testutil.ToFloat64(metrics.SyncTotal.WithLabelValues(string(domain.JobSucceeded))) - beforeOK; got < 1 {
		t.Fatalf("sync_total{succeeded} delta = %v, want >= 1", got)
	}

	// Exposition endpoint serves the registered families.
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"postra_pop3_sync_total", "postra_pop3_messages_fetched_total", "go_goroutines"} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
}
