package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/flipslidersand-labs/dataguard-rail/internal/engine"
	"github.com/flipslidersand-labs/dataguard-rail/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeStore はテスト用の Storer 実装。
type fakeStore struct {
	violations   []engine.Violation
	diff         *store.SchemaDiff
	diffs        []store.SchemaDiff
	diffErr      error
	listDiffsErr error
}

func (f *fakeStore) CountViolations() (int, error) { return len(f.violations), nil }
func (f *fakeStore) ListViolationsPaged(limit, offset int) ([]engine.Violation, error) {
	return pageViolations(f.violations, limit, offset), nil
}
func (f *fakeStore) CountViolationsByTable(table string) (int, error) {
	return len(f.violationsByTable(table)), nil
}
func (f *fakeStore) ListViolationsByTablePaged(table string, limit, offset int) ([]engine.Violation, error) {
	return pageViolations(f.violationsByTable(table), limit, offset), nil
}
func (f *fakeStore) violationsByTable(table string) []engine.Violation {
	matched := make([]engine.Violation, 0)
	for _, v := range f.violations {
		if v.Table == table {
			matched = append(matched, v)
		}
	}
	return matched
}
func pageViolations(all []engine.Violation, limit, offset int) []engine.Violation {
	if offset >= len(all) {
		return []engine.Violation{}
	}
	end := offset + limit
	if limit <= 0 || end > len(all) {
		end = len(all)
	}
	return all[offset:end]
}
func (f *fakeStore) LatestDiff(_ string) (*store.SchemaDiff, error) { return f.diff, f.diffErr }
func (f *fakeStore) ListDiffs() ([]store.SchemaDiff, error)         { return f.diffs, f.listDiffsErr }

// fakeRunner はテスト用の Runner 実装。
type fakeRunner struct{ payload json.RawMessage }

func (f *fakeRunner) Analyze(_ context.Context, _ string) (json.RawMessage, error) {
	return f.payload, nil
}

func newTestServer(st Storer, runner Runner) *Server {
	return New(st, runner, nil, "", nil)
}

func newAuthedTestServer(st Storer, runner Runner, apiKey string) *Server {
	return New(st, runner, nil, apiKey, nil)
}

func TestHealth(t *testing.T) {
	srv := newTestServer(&fakeStore{}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/health", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

func TestViolationsAll(t *testing.T) {
	st := &fakeStore{
		violations: []engine.Violation{
			{ID: "v1", Table: "products", Rule: "r"},
			{ID: "v2", Table: "orders", Rule: "r"},
		},
	}
	srv := newTestServer(st, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/violations", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var got []engine.Violation
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 violations, got %d", len(got))
	}
}

func TestViolationsFilter(t *testing.T) {
	st := &fakeStore{
		violations: []engine.Violation{
			{ID: "v1", Table: "products"},
			{ID: "v2", Table: "orders"},
		},
	}
	srv := newTestServer(st, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/violations?table=products", nil)
	srv.Handler().ServeHTTP(w, req)
	var got []engine.Violation
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 1 || got[0].Table != "products" {
		t.Errorf("unexpected filter result: %+v", got)
	}
}

func TestViolationsPagination(t *testing.T) {
	viols := make([]engine.Violation, 5)
	for i := range viols {
		viols[i] = engine.Violation{ID: fmt.Sprintf("v%d", i), Table: "t"}
	}
	st := &fakeStore{violations: viols}
	srv := newTestServer(st, &fakeRunner{})

	// limit=2&offset=1 → v1,v2
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/violations?limit=2&offset=1", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if w.Header().Get("X-Total-Count") != "5" {
		t.Errorf("X-Total-Count: want 5, got %s", w.Header().Get("X-Total-Count"))
	}
	var got []engine.Violation
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 2 {
		t.Errorf("want 2, got %d", len(got))
	}
}

func TestViolationsPaginationInvalidLimit(t *testing.T) {
	srv := newTestServer(&fakeStore{}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/violations?limit=abc", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestViolationsPaginationInvalidOffset(t *testing.T) {
	srv := newTestServer(&fakeStore{}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/violations?offset=abc", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestViolationsPaginationNegativeOffset(t *testing.T) {
	srv := newTestServer(&fakeStore{}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/violations?offset=-1", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestViolationsPaginationExceedMaxLimit(t *testing.T) {
	srv := newTestServer(&fakeStore{}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/violations?limit=9999", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestLineageMissingParam(t *testing.T) {
	srv := newTestServer(&fakeStore{}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/lineage", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestLineageAbsolutePathRejected(t *testing.T) {
	srv := newTestServer(&fakeStore{}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/lineage?sql=/etc/dataguard/secrets/internal.sql", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestLineageOK(t *testing.T) {
	payload := json.RawMessage(`{"target":"t","sources":["s"],"has_cycle":false}`)
	// /api/lineage は基準ディレクトリ(--sql-dir)配下に実在する .sql だけを受け付ける。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.sql"), []byte("SELECT 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := New(&fakeStore{}, &fakeRunner{payload: payload}, nil, "", nil, WithSQLDir(dir))
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/lineage?sql=x.sql", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if w.Body.String() != string(payload) {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestSchemaDiffAll(t *testing.T) {
	st := &fakeStore{
		diffs: []store.SchemaDiff{{Table: "products", DetectedAt: "2026-01-02T00:00:00Z"}},
	}
	srv := newTestServer(st, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/schema-diff", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

func TestSchemaDiffTable(t *testing.T) {
	diff := &store.SchemaDiff{Table: "products", DetectedAt: "2026-01-02T00:00:00Z",
		Added: []store.ColumnDef{{Name: "discount", Type: "numeric"}},
	}
	srv := newTestServer(&fakeStore{diff: diff}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/schema-diff?table=products", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var got store.SchemaDiff
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Added) != 1 {
		t.Errorf("want 1 added column, got %d", len(got.Added))
	}
}

// failingNotifier は常にエラーを返す alert.Notifier。
type failingNotifier struct{ err error }

func (f failingNotifier) Notify(_ context.Context, _ string) error { return f.err }

// TestSchemaDiffLogsNotifyFailure は notifier.Notify が失敗した際、
// handleSchemaDiff がそのエラーを握りつぶさずログに記録することを確認する（#108）。
func TestSchemaDiffLogsNotifyFailure(t *testing.T) {
	diff := &store.SchemaDiff{
		Table: "products", DetectedAt: "2026-01-02T00:00:00Z",
		Added: []store.ColumnDef{{Name: "discount", Type: "numeric"}},
	}
	notifyErr := fmt.Errorf("slack webhook: 429 too many requests")

	core, logs := observer.New(zap.WarnLevel)
	log := zap.New(core)

	s := New(&fakeStore{diff: diff}, &fakeRunner{}, failingNotifier{err: notifyErr}, "", log)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/schema-diff?table=products", nil)
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	entries := logs.FilterMessage("notify failed").All()
	if len(entries) != 1 {
		t.Fatalf("want 1 'notify failed' log entry, got %d: %+v", len(entries), logs.All())
	}
}

func TestDashboard(t *testing.T) {
	srv := newTestServer(&fakeStore{}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("unexpected Content-Type: %s", ct)
	}
	body := w.Body.String()
	if !containsAll(body, "DataGuard Rail", "/api/violations", "/api/schema-diff") {
		t.Error("dashboard HTML missing expected content")
	}
}

func TestAuthMissingToken(t *testing.T) {
	srv := newAuthedTestServer(&fakeStore{}, &fakeRunner{}, "secret")
	for _, path := range []string{"/api/violations", "/api/schema-diff", "/api/lineage?sql=a.sql"} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, path, nil)
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d", path, w.Code)
		}
	}
}

// ダッシュボードの HTML は静的でデータを含まないため、API キー設定時もキー入力欄を
// 表示できるよう認証なしで配信する。データは認証付きの /api からのみ取得される。
func TestDashboardHTMLServedWithoutKeyAndCarriesNoData(t *testing.T) {
	st := &fakeStore{violations: []engine.Violation{{ID: "v-secret-marker", Table: "t", Rule: "r"}}}
	srv := newAuthedTestServer(st, &fakeRunner{}, "secret")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if containsAll(w.Body.String(), "v-secret-marker") {
		t.Error("dashboard HTML must not embed stored data")
	}
}

func TestDashboardHasKeyInputAndSendsBearerInMemoryOnly(t *testing.T) {
	srv := newAuthedTestServer(&fakeStore{}, &fakeRunner{}, "secret")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	srv.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if !containsAll(body, `id="api-key"`, `type="password"`, "'Authorization': 'Bearer ' + apiKey", "apiFetch('/api/violations')", "apiFetch('/api/schema-diff')") {
		t.Error("dashboard must provide a key input and send it as Authorization: Bearer on fetch")
	}
	for _, banned := range []string{"localStorage", "sessionStorage", "document.cookie"} {
		if containsAll(body, banned) {
			t.Errorf("API key must stay in memory only, found %q", banned)
		}
	}
	if !containsAll(body, "function esc(s)", "${esc(v.rule)}") {
		t.Error("esc() based escaping must be preserved")
	}
}

func TestAuthWrongToken(t *testing.T) {
	srv := newAuthedTestServer(&fakeStore{}, &fakeRunner{}, "secret")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/violations", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAuthCorrectToken(t *testing.T) {
	srv := newAuthedTestServer(&fakeStore{}, &fakeRunner{}, "secret")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/violations", nil)
	req.Header.Set("Authorization", "Bearer secret")
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

func TestAuthHealthAlwaysOpen(t *testing.T) {
	srv := newAuthedTestServer(&fakeStore{}, &fakeRunner{}, "secret")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/health", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestSchemaDiffTableStoreError(t *testing.T) {
	srv := newTestServer(&fakeStore{diffErr: errors.New("boom latest")}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/schema-diff?table=users", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "内部エラーが発生しました" {
		t.Errorf("unexpected error body: %s", w.Body.String())
	}
}

func TestSchemaDiffAllStoreError(t *testing.T) {
	srv := newTestServer(&fakeStore{listDiffsErr: errors.New("boom list")}, &fakeRunner{})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/schema-diff", nil)
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "内部エラーが発生しました" {
		t.Errorf("unexpected error body: %s", w.Body.String())
	}
}
