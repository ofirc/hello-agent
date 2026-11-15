// Command product-catalog is the demo production workload: a hello-world e-commerce
// service that exists so there is something real to drift.
//
// Two things make it useful as a drift target:
//
//   - It reports its own version at /version and in a Prometheus gauge, so the agent's
//     answer can be checked against the pod itself.
//   - The filters feature is gated on the version, which reproduces the scenario from
//     the talk: a customer complains a feature is missing, nothing is crashing, and the
//     only symptom is that prod is one release behind.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	version = envOr("APP_VERSION", "v0.0.0")
	env     = envOr("APP_ENV", "dev")
	service = envOr("APP_SERVICE", "product-catalog")

	// Labelled service/env to match the PromQL the agent's Grafana provider runs.
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests.",
	}, []string{"service", "env", "code", "path"})

	latency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency.",
		Buckets: prometheus.DefBuckets,
	}, []string{"service", "env", "path"})

	buildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "app_build_info",
		Help: "Always 1, labelled with the running version.",
	}, []string{"service", "env", "version"})
)

type product struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Price float64  `json:"price"`
	Tags  []string `json:"tags,omitempty"`
}

var catalog = []product{
	{ID: "p-1", Name: "Mechanical keyboard", Price: 129.00, Tags: []string{"peripherals", "in-stock"}},
	{ID: "p-2", Name: "Standing desk", Price: 549.00, Tags: []string{"furniture", "in-stock"}},
	{ID: "p-3", Name: "Noise-cancelling headphones", Price: 299.00, Tags: []string{"audio", "backorder"}},
}

func main() {
	buildInfo.WithLabelValues(service, env, version).Set(1)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/version", instrument("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"service": service, "env": env, "version": version,
		})
	}))
	mux.HandleFunc("/products", instrument("/products", handleProducts))
	mux.HandleFunc("/", instrument("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"service": service, "env": env, "version": version,
			"hint": "GET /products, /products?filter=in-stock, /version, /healthz, /metrics",
		})
	}))

	port := envOr("PORT", "8080")
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("%s %s starting on :%s (env=%s)", service, version, port, env)
	log.Fatal(srv.ListenAndServe())
}

// handleProducts is where the version gate lives.
//
// The ?filter= parameter only works from v1.2.0 onwards. On v1.1.0 it returns 501, which
// is exactly the "missing feature in production" complaint that opens the talk: the
// service is healthy, the error rate is near zero, and a customer is still right.
func handleProducts(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("filter")
	if filter == "" {
		writeJSON(w, http.StatusOK, catalog)
		return
	}
	if !hasFilters() {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":   "filtering is not available in this version",
			"version": version,
			"hint":    "introduced in v1.2.0",
		})
		return
	}
	var out []product
	for _, p := range catalog {
		for _, t := range p.Tags {
			if t == filter {
				out = append(out, p)
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// hasFilters does a crude major.minor comparison, which is all a demo gate needs.
func hasFilters() bool {
	v := strings.TrimPrefix(version, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	return parts[0] > "0" && (parts[0] > "1" || parts[1] >= "2")
}

func instrument(path string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, code: http.StatusOK}
		next(rec, r)
		latency.WithLabelValues(service, env, path).Observe(time.Since(start).Seconds())
		requests.WithLabelValues(service, env, itoa(rec.code), path).Inc()
	}
}

type recorder struct {
	http.ResponseWriter
	code int
}

func (r *recorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(i int) string {
	return string(rune('0'+i/100)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
