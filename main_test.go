package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func serve(t *testing.T, cfg config, ready bool, path string) *httptest.ResponseRecorder {
	t.Helper()
	var r atomic.Bool
	r.Store(ready)
	rec := httptest.NewRecorder()
	newMux(cfg, "test-host", &r).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestRootOK(t *testing.T) {
	if got := serve(t, config{}, true, "/").Code; got != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", got)
	}
}

func TestRootInjectedFailure(t *testing.T) {
	if got := serve(t, config{failRate: 1}, true, "/").Code; got != http.StatusInternalServerError {
		t.Fatalf("GET / with failRate=1 = %d, want 500", got)
	}
}

func TestRootInjectedLatency(t *testing.T) {
	start := time.Now()
	serve(t, config{latency: 50 * time.Millisecond}, true, "/")
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Fatalf("GET / took %v, want >= 50ms", d)
	}
}

func TestReadyz(t *testing.T) {
	if got := serve(t, config{}, true, "/readyz").Code; got != http.StatusOK {
		t.Fatalf("readyz ready = %d, want 200", got)
	}
	if got := serve(t, config{}, false, "/readyz").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("readyz shutting down = %d, want 503", got)
	}
}

func TestUnknownPath404(t *testing.T) {
	if got := serve(t, config{}, true, "/nope").Code; got != http.StatusNotFound {
		t.Fatalf("GET /nope = %d, want 404", got)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("FAIL_RATE", "0.2")
	t.Setenv("LATENCY_MS", "300")
	c := loadConfig()
	if c.failRate != 0.2 || c.latency != 300*time.Millisecond {
		t.Fatalf("loadConfig = %+v", c)
	}
	t.Setenv("FAIL_RATE", "5") // 超出范围，忽略
	if c := loadConfig(); c.failRate != 0 {
		t.Fatalf("FAIL_RATE=5 should be ignored, got %v", c.failRate)
	}
}
