package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// scalingService is the JSON projection rendered as cards by the frontend.
// Static -- the catalog is built once at startup.
type scalingService struct {
	Name       string  `json:"name"`
	Source     string  `json:"source"`         // "host" or "container"
	AllocCPU   float64 `json:"alloc_cpu"`      // cores
	AllocMemMB int     `json:"alloc_memory_mb"`
}

// scalingStat is the JSON row returned per service on each /api/scaling-stats
// poll. Polled by the frontend every ~5 s.
type scalingStat struct {
	Name           string  `json:"name"`
	CurCPUCores    float64 `json:"cur_cpu_cores"`
	CurMemoryBytes uint64  `json:"cur_memory_bytes"`
	Error          string  `json:"error,omitempty"`
}

// scalingTarget is the internal record: scalingService plus the collector
// metadata (Prometheus job name or container name) the public struct hides.
type scalingTarget struct {
	scalingService
	PromJob       string // for source=host
	ContainerName string // for source=container
}

type scalingCatalog struct {
	targets         []scalingTarget
	prometheusURL   string
	httpClient      *http.Client
	isDockerDesktop bool
}

// scalingEnv is the small environment block returned alongside the catalog so
// the frontend can render a "Docker Desktop" badge and tooltip explaining
// why container CPU% can read above 100% of the allocated cap.
type scalingEnv struct {
	DockerDesktop bool `json:"docker_desktop"`
}

// detectDockerDesktop runs `docker info --format {{.OperatingSystem}}` once at
// startup. On macOS/Windows the value is "Docker Desktop" (sometimes with a
// version suffix); on stock Linux Docker it's "Ubuntu 22.04" / "Alpine ..."
// etc. Best-effort -- failure (docker not on PATH, daemon down) returns false.
func detectDockerDesktop() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.OperatingSystem}}").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "docker desktop")
}

// limitsFile mirrors enough of config/local-limits.yaml for the loadtest
// binary to know what's allocated per service. cmd/limits owns the full
// schema (validation, rendering); duplicating just the read-side types here
// avoids a cross-package refactor since both cmd/limits and cmd/loadtest
// are package main.
type limitsFile struct {
	Services map[string]struct {
		CPU      float64 `yaml:"cpu"`
		MemoryMB int     `yaml:"memory_mb"`
	} `yaml:"services"`
}

// hostBinaryJobs map a Prometheus job= label to the logical service name.
// The job names match deploy/prometheus/prometheus.yml. loadtest is the page
// host itself, so it doesn't get its own scrape; we skip it.
var hostBinaryJobs = []string{"api", "worker", "observer"}

// composeContainers map logical service names to the docker container name
// Compose produces (project + service + index). Compose's project name is
// `shortlink` per docker-compose.yml's `name:` field.
var composeContainers = map[string]string{
	"postgres":   "shortlink-postgres-1",
	"redis":      "shortlink-redis-1",
	"minio":      "shortlink-minio-1",
	"pgbouncer":  "shortlink-pgbouncer-1",
	"prometheus": "shortlink-prometheus-1",
	"grafana":    "shortlink-grafana-1",
}

// loadScalingCatalog reads local-limits.yaml and pairs each known service
// with its collector metadata. Services in the YAML but absent from both
// lookup maps are skipped.
func loadScalingCatalog(limitsPath, prometheusURL string) (*scalingCatalog, error) {
	data, err := os.ReadFile(limitsPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", limitsPath, err)
	}
	var f limitsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", limitsPath, err)
	}

	c := &scalingCatalog{
		prometheusURL:   prometheusURL,
		httpClient:      &http.Client{Timeout: 3 * time.Second},
		isDockerDesktop: detectDockerDesktop(),
	}
	for _, job := range hostBinaryJobs {
		s, ok := f.Services[job]
		if !ok {
			continue
		}
		c.targets = append(c.targets, scalingTarget{
			scalingService: scalingService{Name: job, Source: "host", AllocCPU: s.CPU, AllocMemMB: s.MemoryMB},
			PromJob:        job,
		})
	}
	var ckeys []string
	for k := range composeContainers {
		if _, ok := f.Services[k]; ok {
			ckeys = append(ckeys, k)
		}
	}
	sort.Strings(ckeys)
	for _, k := range ckeys {
		s := f.Services[k]
		c.targets = append(c.targets, scalingTarget{
			scalingService: scalingService{Name: k, Source: "container", AllocCPU: s.CPU, AllocMemMB: s.MemoryMB},
			ContainerName:  composeContainers[k],
		})
	}
	return c, nil
}

// servicesHandler serves GET /api/scaling-services -- the static catalog plus
// the runtime env block (Docker Desktop flag) so the frontend can decorate
// container CPU% with the right caveat.
func (c *scalingCatalog) servicesHandler(w http.ResponseWriter, r *http.Request) {
	out := make([]scalingService, 0, len(c.targets))
	for _, t := range c.targets {
		out = append(out, t.scalingService)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"services": out,
		"env":      scalingEnv{DockerDesktop: c.isDockerDesktop},
	})
}

// statsHandler serves GET /api/scaling-stats -- current CPU + memory per
// service. Collects Prometheus and docker stats in parallel; either failing
// returns an error-bearing row rather than failing the whole response.
func (c *scalingCatalog) statsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	type result struct {
		stats []scalingStat
	}
	hostCh := make(chan result, 1)
	containerCh := make(chan result, 1)

	go func() {
		hostCh <- result{stats: c.collectHostStats(ctx)}
	}()
	go func() {
		containerCh <- result{stats: c.collectContainerStats(ctx)}
	}()

	out := make([]scalingStat, 0, len(c.targets))
	out = append(out, (<-hostCh).stats...)
	out = append(out, (<-containerCh).stats...)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"stats": out})
}

// collectHostStats batches two PromQL queries (one for memory, one for CPU
// rate) across all host-binary jobs. Per-job decompose happens in the
// response loop. Failure → all host services get an Error row.
func (c *scalingCatalog) collectHostStats(ctx context.Context) []scalingStat {
	jobs := make([]string, 0)
	for _, t := range c.targets {
		if t.Source == "host" {
			jobs = append(jobs, t.PromJob)
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	jobsAlt := strings.Join(jobs, "|")
	cpuQuery := fmt.Sprintf(`rate(process_cpu_seconds_total{job=~"%s"}[1m])`, jobsAlt)
	memQuery := fmt.Sprintf(`process_resident_memory_bytes{job=~"%s"}`, jobsAlt)

	cpuByJob, cpuErr := c.queryProm(ctx, cpuQuery)
	memByJob, memErr := c.queryProm(ctx, memQuery)

	out := make([]scalingStat, 0, len(jobs))
	for _, job := range jobs {
		st := scalingStat{Name: job}
		switch {
		case cpuErr != nil:
			st.Error = "prom cpu: " + cpuErr.Error()
		case memErr != nil:
			st.Error = "prom mem: " + memErr.Error()
		default:
			st.CurCPUCores = cpuByJob[job]
			st.CurMemoryBytes = uint64(memByJob[job])
		}
		out = append(out, st)
	}
	return out
}

// queryProm runs a single PromQL instant query and returns a map from the
// `job` label to the numeric value. The shape works for both vector and
// rate() results since we don't aggregate -- one series per job.
func (c *scalingCatalog) queryProm(ctx context.Context, query string) (map[string]float64, error) {
	u := c.prometheusURL + "/api/v1/query?query=" + url.QueryEscape(query)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prom returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	out := make(map[string]float64, len(parsed.Data.Result))
	for _, r := range parsed.Data.Result {
		job := r.Metric["job"]
		// value[1] is a string per Prom JSON contract.
		s, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		out[job] = v
	}
	return out, nil
}

// dockerStatLine is one entry of `docker stats --no-stream --format "{{json .}}"`.
type dockerStatLine struct {
	Name     string `json:"Name"`
	CPUPerc  string `json:"CPUPerc"`  // "0.02%"
	MemUsage string `json:"MemUsage"` // "35.1MiB / 512MiB"
}

// collectContainerStats invokes `docker stats --no-stream` once for ALL
// containers and matches by container name. On Docker Desktop this is the
// reliable path -- cAdvisor's overlay-layer enumeration fails there. The
// CLI is already a prereq of scripts/local-setup, so no new dependency.
func (c *scalingCatalog) collectContainerStats(ctx context.Context) []scalingStat {
	containers := make([]scalingTarget, 0)
	for _, t := range c.targets {
		if t.Source == "container" {
			containers = append(containers, t)
		}
	}
	if len(containers) == 0 {
		return nil
	}

	cmd := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{json .}}")
	out, err := cmd.Output()
	if err != nil {
		// Emit an Error row per container rather than dropping them silently.
		errs := make([]scalingStat, 0, len(containers))
		for _, t := range containers {
			errs = append(errs, scalingStat{Name: t.Name, Error: "docker stats: " + err.Error()})
		}
		return errs
	}

	byContainer := make(map[string]dockerStatLine)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var d dockerStatLine
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			continue
		}
		byContainer[d.Name] = d
	}

	stats := make([]scalingStat, 0, len(containers))
	for _, t := range containers {
		d, ok := byContainer[t.ContainerName]
		if !ok {
			stats = append(stats, scalingStat{Name: t.Name, Error: "container not running"})
			continue
		}
		cpuPct, _ := strconv.ParseFloat(strings.TrimSuffix(d.CPUPerc, "%"), 64)
		usedBytes, _ := parseMemUsage(d.MemUsage)
		stats = append(stats, scalingStat{
			Name:           t.Name,
			CurCPUCores:    cpuPct / 100.0, // CPUPerc is % of one core
			CurMemoryBytes: usedBytes,
		})
	}
	return stats
}

// parseMemUsage parses docker stats' "35.1MiB / 512MiB" into bytes for the
// first half. Returns the parsed byte count and an error if either the
// number or the unit can't be read.
func parseMemUsage(s string) (uint64, error) {
	left := strings.TrimSpace(strings.SplitN(s, "/", 2)[0])
	// Split number and unit at the first letter.
	i := strings.IndexFunc(left, func(r rune) bool {
		return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
	})
	if i < 0 {
		return 0, fmt.Errorf("no unit in %q", s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(left[:i]), 64)
	if err != nil {
		return 0, fmt.Errorf("number %q: %w", left[:i], err)
	}
	unit := strings.ToUpper(strings.TrimSpace(left[i:]))
	var mul float64
	switch unit {
	case "B":
		mul = 1
	case "KB", "KIB", "K":
		mul = 1024
	case "MB", "MIB", "M":
		mul = 1024 * 1024
	case "GB", "GIB", "G":
		mul = 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown unit %q in %q", unit, s)
	}
	return uint64(n * mul), nil
}

