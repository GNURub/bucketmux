package httpserver

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gnurub/bucketmux/internal/admin"
	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/gateway"
)

func NewHTTPHandler(svc *app.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(svc.PrometheusMetrics(r.Context())))
	})
	mux.Handle("/uppy/s3/", gateway.NewUppyHandler(svc))
	if svc.Config.Admin.Enabled {
		mux.Handle("/admin", admin.NewHandler(svc))
		mux.Handle("/admin/", admin.NewHandler(svc))
	}
	mux.Handle("/", gateway.NewHandler(svc))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
