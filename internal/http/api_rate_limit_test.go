package http

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIRateLimiter_AllowsUnderLimit(t *testing.T) {
	l := newAPIRateLimiter()

	for i := range apiRateLimitPerIP {
		if !l.limiterFor("1.2.3.4").Allow() {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
}

func TestAPIRateLimiter_BlocksOverLimit(t *testing.T) {
	l := newAPIRateLimiter()

	for range apiRateLimitPerIP {
		l.limiterFor("1.2.3.4").Allow()
	}

	if l.limiterFor("1.2.3.4").Allow() {
		t.Error("request over limit should be blocked")
	}
}

func TestAPIRateLimiter_IndependentPerIP(t *testing.T) {
	l := newAPIRateLimiter()

	// Exhaust limit for one IP
	for range apiRateLimitPerIP {
		l.limiterFor("1.2.3.4").Allow()
	}

	// Different IP should still be allowed
	if !l.limiterFor("5.6.7.8").Allow() {
		t.Error("different IP should not be affected")
	}
}

func TestAPIRateLimiter_EmptyIPAllowed(t *testing.T) {
	l := newAPIRateLimiter()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := l.RateLimit(inner)

	req := httptest.NewRequest("GET", "/api/stats", nil)
	req.RemoteAddr = ""
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("empty IP should always be allowed, got %d", w.Code)
	}
}

func TestAPIRateLimiter_Middleware(t *testing.T) {
	l := newAPIRateLimiter()
	called := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})
	handler := l.RateLimit(inner)

	// Use varying source ports to verify the middleware normalizes to IP only.
	for i := range apiRateLimitPerIP + 2 {
		req := httptest.NewRequest("GET", "/api/stats", nil)
		req.RemoteAddr = fmt.Sprintf("10.0.0.1:%d", 10000+i)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if i < apiRateLimitPerIP {
			if w.Code != http.StatusOK {
				t.Fatalf("request %d: got %d, want 200", i+1, w.Code)
			}
		} else {
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("request %d: got %d, want 429", i+1, w.Code)
			}
		}
	}

	if called != apiRateLimitPerIP {
		t.Errorf("inner handler called %d times, want %d", called, apiRateLimitPerIP)
	}
}

func TestAPIRateLimiter_Sweep(t *testing.T) {
	l := newAPIRateLimiter()

	l.limiterFor("1.2.3.4")

	// Manually set lastSeen far in the past to trigger sweep
	l.mu.Lock()
	l.limiters["1.2.3.4"].lastSeen = l.limiters["1.2.3.4"].lastSeen.Add(-(apiRateLimiterSweepEvery + apiRateLimitWindow*2 + 1))
	l.lastCleanup = l.limiters["1.2.3.4"].lastSeen
	l.mu.Unlock()

	l.limiterFor("5.6.7.8") // triggers sweep

	l.mu.Lock()
	_, exists := l.limiters["1.2.3.4"]
	l.mu.Unlock()

	if exists {
		t.Error("stale entry should have been swept")
	}
}
