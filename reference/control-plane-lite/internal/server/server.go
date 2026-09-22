package server

import (
	"encoding/json"
	"net/http"
)

const serviceName = "arop-reference-control-plane"

// NewHandler returns the HTTP surface implemented by the reference control
// plane. Only process health is exposed in the repository-baseline phase.
func NewHandler(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health/live", healthHandler("ok", version))
	mux.HandleFunc("GET /v1/health/ready", healthHandler("ready", version))
	return securityHeaders(mux)
}

type healthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

func healthHandler(status, version string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, healthResponse{
			Status:  status,
			Service: serviceName,
			Version: version,
		})
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
