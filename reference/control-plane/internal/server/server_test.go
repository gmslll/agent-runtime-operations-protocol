package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		path       string
		wantStatus string
	}{
		{name: "live", path: "/v1/health/live", wantStatus: "ok"},
		{name: "ready", path: "/v1/health/ready", wantStatus: "ready"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequest(http.MethodGet, testCase.path, nil)
			response := httptest.NewRecorder()

			NewHandler("0.1.0-dev").ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if got := response.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}

			var body healthResponse
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Status != testCase.wantStatus {
				t.Errorf("status body = %q, want %q", body.Status, testCase.wantStatus)
			}
			if body.Service != serviceName {
				t.Errorf("service = %q, want %q", body.Service, serviceName)
			}
			if body.Version != "0.1.0-dev" {
				t.Errorf("version = %q, want 0.1.0-dev", body.Version)
			}
		})
	}
}

func TestHealthRejectsUnsupportedMethod(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "/v1/health/live", nil)
	response := httptest.NewRecorder()

	NewHandler("0.1.0-dev").ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}
