//go:build integration

// Package integration_test exercises the full ShortLink pipeline end-to-end
// against ephemeral Postgres / Redis / MinIO containers (SPEC §17 M9). The
// api and worker are run as the actual compiled binaries to keep the test
// faithful to what ships.
//
// Run with:
//
//	go test -tags integration ./tests/...
//
// or `make test-integration`. Requires a Docker daemon for testcontainers.
package integration_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" with database/sql
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leninboccardo/shortlink/migrations"
)

// Module-level state, populated by TestMain so subtests share one stack.
var (
	apiBinary    string
	workerBinary string

	pgURL    string
	redisURL string

	minioEndpoint  string
	minioAccessKey = "minioadmin"
	minioSecretKey = "minioadmin"
	minioBucket    = "shortlink-qr"

	apiBaseURL string

	// Two keys: one high-limit for normal tests, one low-limit for the
	// rate-limit test. Both share the same webhook HMAC secret so the
	// sink validation is uniform.
	keyHighLimit  = "sl_live_inthigh_xxxxxxxxxxxxxxxxxxxx"
	keyLowLimit   = "sl_live_intlow__xxxxxxxxxxxxxxxxxxxx"
	webhookSecret = "wh_secret_integration_test_value_pad_to_64_bytes______________"

	sinkServer *webhookSink
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	// 1. Spin up the three infra containers in parallel.
	pgC, pgConn := mustStartPostgres(ctx)
	defer pgC.Terminate(ctx)

	redisC, redisConn := mustStartRedis(ctx)
	defer redisC.Terminate(ctx)

	minioC, minioHostPort := mustStartMinIO(ctx)
	defer minioC.Terminate(ctx)

	pgURL = pgConn
	redisURL = redisConn
	minioEndpoint = minioHostPort

	// 2. Apply migrations to the test Postgres.
	if err := applyMigrations(pgURL); err != nil {
		fmt.Fprintln(os.Stderr, "migrations:", err)
		os.Exit(2)
	}

	// 3. Seed two API keys with known raw values so the test can sign
	//    requests without going through cmd/keygen.
	if err := seedKeys(ctx, pgURL); err != nil {
		fmt.Fprintln(os.Stderr, "seed keys:", err)
		os.Exit(2)
	}

	// 4. Create the MinIO bucket the worker will upload QR PNGs into.
	if err := ensureBucket(ctx, minioEndpoint, minioAccessKey, minioSecretKey, minioBucket); err != nil {
		fmt.Fprintln(os.Stderr, "create bucket:", err)
		os.Exit(2)
	}

	// 5. Stand up the webhook sink. Lives at 127.0.0.1:NNNN; the api / worker
	//    (running on the host) reach it via the same address.
	sinkServer = newWebhookSink()
	defer sinkServer.Close()

	// 6. Build the api + worker binaries into a tmpdir so the test runs
	//    against the exact code that ships, not an in-process refactor.
	tmpDir, err := os.MkdirTemp("", "shortlink-it-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tmpdir:", err)
		os.Exit(2)
	}
	defer os.RemoveAll(tmpDir)

	apiBinary = filepath.Join(tmpDir, binaryName("api"))
	workerBinary = filepath.Join(tmpDir, binaryName("worker"))
	if err := buildBinary("./cmd/api", apiBinary); err != nil {
		fmt.Fprintln(os.Stderr, "build api:", err)
		os.Exit(2)
	}
	if err := buildBinary("./cmd/worker", workerBinary); err != nil {
		fmt.Fprintln(os.Stderr, "build worker:", err)
		os.Exit(2)
	}

	// 7. Launch api + worker against the ephemeral infra.
	apiPort, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "free port:", err)
		os.Exit(2)
	}
	apiBaseURL = fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	workerPort, _ := freePort()

	env := commonEnv(apiPort, workerPort)

	apiProc := startProcess(apiBinary, env)
	defer apiProc.stop()
	workerProc := startProcess(workerBinary, env)
	defer workerProc.stop()

	if err := waitForHealthz(apiBaseURL+"/healthz", 30*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "api not ready:", err)
		apiProc.dumpLogs()
		os.Exit(2)
	}

	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Container helpers

func mustStartPostgres(ctx context.Context) (testcontainers.Container, string) {
	c, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("shortlink"),
		tcpostgres.WithUsername("shortlink"),
		tcpostgres.WithPassword("shortlink"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		os.Exit(2)
	}
	connStr, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres conn:", err)
		os.Exit(2)
	}
	return c, connStr
}

func mustStartRedis(ctx context.Context) (testcontainers.Container, string) {
	c, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		fmt.Fprintln(os.Stderr, "start redis:", err)
		os.Exit(2)
	}
	connStr, err := c.ConnectionString(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "redis conn:", err)
		os.Exit(2)
	}
	return c, connStr
}

func mustStartMinIO(ctx context.Context) (testcontainers.Container, string) {
	c, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(minioAccessKey),
		tcminio.WithPassword(minioSecretKey),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start minio:", err)
		os.Exit(2)
	}
	endpoint, err := c.Endpoint(ctx, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "minio endpoint:", err)
		os.Exit(2)
	}
	return c, endpoint
}

func applyMigrations(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	// Use the same embed.FS cmd/migrate ships with so the integration test
	// exercises the actual migration source baked into the binary, not the
	// on-disk tree.
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}

// seedKeys inserts two API keys: one pro-tier with a high limit, one free-tier
// with a low limit. Mirrors what cmd/keygen does, but without the YAML file
// since the test owns the raw values.
func seedKeys(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, k := range []struct {
		raw, tier, name, hint string
	}{
		{keyHighLimit, "pro", "integration-high", keyHint(keyHighLimit)},
		{keyLowLimit, "free", "integration-low", keyHint(keyLowLimit)},
	} {
		hash := sha256Hex(k.raw)
		_, err := db.ExecContext(ctx, `
			INSERT INTO api_keys (key_hash, key_hint, name, tier, webhook_secret)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (key_hash) DO NOTHING
		`, hash, k.hint, k.name, k.tier, webhookSecret)
		if err != nil {
			return fmt.Errorf("insert %s: %w", k.hint, err)
		}
	}
	return nil
}

func ensureBucket(ctx context.Context, endpoint, ak, sk, bucket string) error {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(ak, sk, ""),
		Secure: false,
	})
	if err != nil {
		return err
	}
	exists, err := mc.BucketExists(ctx, bucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
}

// ---------------------------------------------------------------------------
// Binary build + launch helpers

func binaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func buildBinary(pkg, out string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = root
	out2, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build %s: %w\n%s", pkg, err, out2)
	}
	return nil
}

func commonEnv(apiPort, workerPort int) []string {
	return append(os.Environ(),
		"DATABASE_URL="+pgURL,
		"REDIS_URL="+redisURL,
		"MINIO_ENDPOINT="+minioEndpoint,
		"MINIO_ACCESS_KEY="+minioAccessKey,
		"MINIO_SECRET_KEY="+minioSecretKey,
		"MINIO_BUCKET="+minioBucket,
		"MINIO_USE_SSL=false",
		fmt.Sprintf("API_PORT=%d", apiPort),
		fmt.Sprintf("WORKER_PORT=%d", workerPort),
		"SHORT_URL_BASE="+apiBaseURL,
		"RATE_LIMIT_FREE=3",
		"RATE_LIMIT_PRO=200",
		// Allowlist the loopback so the SSRF guard lets webhook deliveries
		// reach the local sink; deliberately NOT including 10.0.0.0/8 so
		// the SSRF test can use a private-range URL to assert rejection.
		"SSRF_ALLOWLIST=127.0.0.1,localhost",
		// Empty observer URL = best-effort emits drop silently (no observer
		// is running in this test).
		"OBSERVER_URL=",
		"LOG_LEVEL=warn",
	)
}

type process struct {
	cmd *exec.Cmd
	buf *bytes.Buffer
	mu  sync.Mutex
}

func startProcess(bin string, env []string) *process {
	cmd := exec.Command(bin)
	cmd.Env = env
	buf := &bytes.Buffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start", bin, err)
		os.Exit(2)
	}
	return &process{cmd: cmd, buf: buf}
}

func (p *process) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	// SIGTERM first (graceful), then kill if it doesn't exit in 5s.
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
}

func (p *process) dumpLogs() {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(os.Stderr, "---", p.cmd.Path, "stdout/stderr ---")
	fmt.Fprintln(os.Stderr, p.buf.String())
}

func waitForHealthz(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("healthz never reached: %s", url)
		}
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Webhook sink

type webhookDelivery struct {
	Body      []byte
	Signature string
	KeyHint   string
	JobID     string
	Status    int // status we returned
}

type webhookSink struct {
	server *httptest.Server
	url    string

	mu       sync.Mutex
	channels map[string]chan webhookDelivery // job_id -> 1-element chan
	// firstAttemptStatus lets a test ask the sink to fail the first delivery
	// for a job_id with a transient 5xx and succeed afterward.
	firstAttemptStatus map[string]int
}

func newWebhookSink() *webhookSink {
	s := &webhookSink{
		channels:           map[string]chan webhookDelivery{},
		firstAttemptStatus: map[string]int{},
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	s.url = s.server.URL
	return s
}

func (s *webhookSink) Close() { s.server.Close() }

// channelLocked returns the buffered channel for jobID, creating it on demand.
// Caller must hold s.mu. Auto-creation closes the race where the worker
// delivers a webhook before the test gets a chance to register interest in it.
func (s *webhookSink) channelLocked(jobID string) chan webhookDelivery {
	if ch, ok := s.channels[jobID]; ok {
		return ch
	}
	ch := make(chan webhookDelivery, 4)
	s.channels[jobID] = ch
	return ch
}

// expect returns the channel that will receive future (and any already-buffered)
// deliveries for jobID. Safe to call before or after the delivery arrives.
func (s *webhookSink) expect(jobID string) chan webhookDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channelLocked(jobID)
}

func (s *webhookSink) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	jobID := r.Header.Get("X-ShortLink-Job-ID")

	s.mu.Lock()
	status := http.StatusOK
	if want, ok := s.firstAttemptStatus[jobID]; ok {
		status = want
		// Subsequent attempts succeed.
		delete(s.firstAttemptStatus, jobID)
	}
	ch := s.channelLocked(jobID)
	s.mu.Unlock()

	w.WriteHeader(status)
	select {
	case ch <- webhookDelivery{
		Body:      body,
		Signature: r.Header.Get("X-ShortLink-Signature"),
		KeyHint:   r.Header.Get("X-ShortLink-Key-Hint"),
		JobID:     jobID,
		Status:    status,
	}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Tests

func TestShortenHappyPath(t *testing.T) {
	body := postShorten(t, keyHighLimit, "https://example.com/long-url", sinkServer.url)
	if body.StatusCode != http.StatusAccepted {
		t.Fatalf("got status %d, want 202", body.StatusCode)
	}
	jobID := body.JobID
	if jobID == "" {
		t.Fatal("response missing job_id")
	}

	delivery := waitForDelivery(t, jobID, 20*time.Second)
	if delivery.Status != http.StatusOK {
		t.Fatalf("sink returned %d, want 200", delivery.Status)
	}

	// HMAC verification.
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(delivery.Body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if delivery.Signature != want {
		t.Fatalf("signature mismatch:\n got %s\nwant %s", delivery.Signature, want)
	}

	// Payload shape.
	var payload struct {
		JobID       string `json:"job_id"`
		Status      string `json:"status"`
		ShortURL    string `json:"short_url"`
		OriginalURL string `json:"original_url"`
		QRCode      struct {
			DownloadURL string `json:"download_url"`
			SizeBytes   int64  `json:"size_bytes"`
		} `json:"qr_code"`
	}
	if err := json.Unmarshal(delivery.Body, &payload); err != nil {
		t.Fatalf("parse webhook body: %v\n%s", err, string(delivery.Body))
	}
	if payload.JobID != jobID {
		t.Fatalf("payload job_id mismatch: got %s want %s", payload.JobID, jobID)
	}
	if payload.Status != "success" {
		t.Fatalf("payload status %q, want success", payload.Status)
	}
	if payload.OriginalURL != "https://example.com/long-url" {
		t.Fatalf("original_url mismatch: %s", payload.OriginalURL)
	}
	if payload.ShortURL == "" || !strings.HasPrefix(payload.ShortURL, apiBaseURL+"/") {
		t.Fatalf("short_url malformed: %s", payload.ShortURL)
	}
	if payload.QRCode.DownloadURL == "" {
		t.Fatal("payload missing qr_code.download_url")
	}

	// The QR PNG should be reachable via the signed URL and look like a PNG.
	resp, err := http.Get(payload.QRCode.DownloadURL)
	if err != nil {
		t.Fatalf("fetch qr: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("qr GET %d, want 200", resp.StatusCode)
	}
	png, _ := io.ReadAll(resp.Body)
	if len(png) < 8 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("qr response is not a PNG (first 8 bytes: %x)", png[:min(8, len(png))])
	}
}

func TestShortenRejectsBadAPIKey(t *testing.T) {
	resp := postShortenRaw(t, "sl_live_bogus_doesnotexist", "https://example.com", sinkServer.url)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
}

func TestShortenRateLimited(t *testing.T) {
	// keyLowLimit is configured for 3 req/min (RATE_LIMIT_FREE=3). Burst 5
	// requests and assert at least one 429 lands.
	var n429 int
	for i := 0; i < 5; i++ {
		resp := postShortenRaw(t, keyLowLimit, "https://example.com/burst", sinkServer.url)
		if resp.StatusCode == http.StatusTooManyRequests {
			n429++
		}
		resp.Body.Close()
	}
	if n429 == 0 {
		t.Fatalf("expected at least one 429 across 5 burst requests, got none")
	}
	// Verify the standard headers are present on the 429 response.
	resp := postShortenRaw(t, keyLowLimit, "https://example.com/burst2", sinkServer.url)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Skipf("burst already drained, skip header check (status=%d)", resp.StatusCode)
	}
	for _, h := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("429 missing %s header", h)
		}
	}
}

func TestShortenRejectsSSRFBlockedWebhook(t *testing.T) {
	// 10.0.0.99 is RFC1918 and NOT in SSRF_ALLOWLIST=127.0.0.1,localhost.
	// The validator should reject this URL with 422 at request time.
	resp := postShortenRaw(t, keyHighLimit, "https://example.com/ssrf", "http://10.0.0.99:8080/sink")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 422 (body=%s)", resp.StatusCode, string(body))
	}
}

// ---------------------------------------------------------------------------
// Small helpers used by tests

type shortenResponse struct {
	StatusCode int
	JobID      string
}

func postShorten(t *testing.T, apiKey, target, webhookURL string) shortenResponse {
	t.Helper()
	resp := postShortenRaw(t, apiKey, target, webhookURL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Logf("non-202 response: %s", string(body))
		return shortenResponse{StatusCode: resp.StatusCode}
	}
	var out struct {
		JobID   string `json:"job_id"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return shortenResponse{StatusCode: resp.StatusCode, JobID: out.JobID}
}

func postShortenRaw(t *testing.T, apiKey, target, webhookURL string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"url":         target,
		"webhook_url": webhookURL,
	})
	req, _ := http.NewRequest(http.MethodPost, apiBaseURL+"/shorten", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /shorten: %v", err)
	}
	return resp
}

func waitForDelivery(t *testing.T, jobID string, timeout time.Duration) webhookDelivery {
	t.Helper()
	ch := sinkServer.expect(jobID)
	select {
	case d := <-ch:
		return d
	case <-time.After(timeout):
		t.Fatalf("webhook for %s never arrived (timeout %s)", jobID, timeout)
		return webhookDelivery{}
	}
}

// repoRoot walks up from this test file to the module root (where go.mod is).
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func keyHint(raw string) string {
	if len(raw) <= 12 {
		return raw
	}
	return raw[:8] + "…" + raw[len(raw)-4:]
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

