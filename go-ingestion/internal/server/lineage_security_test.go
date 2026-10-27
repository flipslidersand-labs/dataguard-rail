package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flipslidersand-labs/dataguard-rail/internal/engine"
	"github.com/flipslidersand-labs/dataguard-rail/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// recordingRunner は Analyze に渡されたパスを記録する Runner。
type recordingRunner struct {
	mu      sync.Mutex
	paths   []string
	err     error
	entered chan struct{} // 非 nil なら Analyze 開始時に通知する
	release chan struct{} // 非 nil なら close されるまで Analyze をブロックする
}

func (r *recordingRunner) Analyze(_ context.Context, p string) (json.RawMessage, error) {
	r.mu.Lock()
	r.paths = append(r.paths, p)
	r.mu.Unlock()
	if r.entered != nil {
		r.entered <- struct{}{}
	}
	if r.release != nil {
		<-r.release
	}
	if r.err != nil {
		return nil, r.err
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (r *recordingRunner) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

// sqlFixture は基準ディレクトリと、その外側のディレクトリを用意する。
//
//	base/ok.sql, base/sub/ok.sql, base/notes.txt, base/dir.sql/(ディレクトリ)
//	base/link.sql -> outside/secret.sql          (外へ出るシンボリックリンク)
//	base/linkdir  -> outside/                    (外へ出るディレクトリのシンボリックリンク)
//	base/alias.sql -> base/sub/ok.sql            (内側に留まるシンボリックリンク)
//	base/txt.sql  -> base/notes.txt              (解決後が .sql でない)
func sqlFixture(t *testing.T) (base string) {
	t.Helper()
	base = t.TempDir()
	outside := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(base, "sub"), 0o700))
	must(os.MkdirAll(filepath.Join(base, "dir.sql"), 0o700))
	must(os.WriteFile(filepath.Join(base, "ok.sql"), []byte("SELECT 1"), 0o600))
	must(os.WriteFile(filepath.Join(base, "sub", "ok.sql"), []byte("SELECT 1"), 0o600))
	must(os.WriteFile(filepath.Join(base, "notes.txt"), []byte("x"), 0o600))
	must(os.WriteFile(filepath.Join(outside, "secret.sql"), []byte("SELECT 2"), 0o600))
	must(os.Symlink(filepath.Join(outside, "secret.sql"), filepath.Join(base, "link.sql")))
	must(os.Symlink(outside, filepath.Join(base, "linkdir")))
	must(os.Symlink(filepath.Join(base, "sub", "ok.sql"), filepath.Join(base, "alias.sql")))
	must(os.Symlink(filepath.Join(base, "notes.txt"), filepath.Join(base, "txt.sql")))
	return base
}

func getLineage(srv *Server, sql string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/lineage?sql="+url.QueryEscape(sql), nil)
	srv.Handler().ServeHTTP(w, req)
	return w
}

func TestLineageRejectsPathsOutsideBaseOrMalformed(t *testing.T) {
	base := sqlFixture(t)
	for _, p := range []string{
		"../outside/secret.sql",
		"link.sql",           // 外を指すシンボリックリンク(ファイル)
		"linkdir/secret.sql", // 外を指すシンボリックリンク(ディレクトリ)
		"txt.sql",            // 解決後が .sql ではない
		"dir.sql",            // ディレクトリ
		"-x.sql",             // 先頭が "-"
		"./-x.sql",
		"notes.txt", // .sql 以外
		"missing.sql",
		"/etc/hosts.sql",
	} {
		rr := &recordingRunner{}
		srv := New(&fakeStore{}, rr, nil, "", nil, WithSQLDir(base))
		w := getLineage(srv, p)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%q: want 400, got %d", p, w.Code)
		}
		if len(rr.calls()) != 0 {
			t.Errorf("%q: runner must not be called, got %v", p, rr.calls())
		}
	}
}

func TestLineageNeverPassesRawSQLForMissingPath(t *testing.T) {
	base := sqlFixture(t)
	rr := &recordingRunner{}
	srv := New(&fakeStore{}, rr, nil, "", nil, WithSQLDir(base))
	// 存在しないパス(SQL 文字列そのものを含む)が Runner まで届かないこと。
	for _, p := range []string{"nope.sql", "DROP TABLE users;.sql", "SELECT 1 FROM t.sql"} {
		if w := getLineage(srv, p); w.Code != http.StatusBadRequest {
			t.Errorf("%q: want 400, got %d", p, w.Code)
		}
	}
	if len(rr.calls()) != 0 {
		t.Errorf("runner must not be called for missing files: %v", rr.calls())
	}
}

func TestLineageAcceptsFilesUnderBaseAndPassesResolvedRelativePath(t *testing.T) {
	base := sqlFixture(t)
	cases := map[string]string{
		"ok.sql":     "ok.sql",
		"sub/ok.sql": "sub/ok.sql",
		"alias.sql":  "sub/ok.sql", // 内側に留まるシンボリックリンクは解決後の相対パスで渡る
	}
	for in, want := range cases {
		rr := &recordingRunner{}
		srv := New(&fakeStore{}, rr, nil, "", nil, WithSQLDir(base))
		if w := getLineage(srv, in); w.Code != http.StatusOK {
			t.Fatalf("%q: want 200, got %d: %s", in, w.Code, w.Body.String())
		}
		if got := rr.calls(); len(got) != 1 || got[0] != want {
			t.Errorf("%q: runner got %v, want [%s]", in, got, want)
		}
	}
}

func TestLineageConcurrencyLimit(t *testing.T) {
	base := sqlFixture(t)
	rr := &recordingRunner{entered: make(chan struct{}, 8), release: make(chan struct{})}
	srv := New(&fakeStore{}, rr, nil, "", nil, WithSQLDir(base), WithMaxLineageConcurrency(2))

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- getLineage(srv, "ok.sql").Code
		}()
	}
	<-rr.entered
	<-rr.entered // 2 件とも Analyze 内でブロック中

	over := make(chan int, 1)
	go func() { over <- getLineage(srv, "ok.sql").Code }()
	select {
	case code := <-over:
		if code != http.StatusTooManyRequests {
			t.Errorf("over the limit: want 429, got %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Error("over the limit: request was not rejected (it is waiting for a slot)")
	}
	if len(rr.calls()) != 2 {
		t.Errorf("rejected request must not start a subprocess: calls=%v", rr.calls())
	}

	close(rr.release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Errorf("in-flight request: want 200, got %d", c)
		}
	}
	// 枠が解放されたら再び受け付ける。
	rr2 := &recordingRunner{}
	srv2 := New(&fakeStore{}, rr2, nil, "", nil, WithSQLDir(base), WithMaxLineageConcurrency(1))
	for i := 0; i < 3; i++ {
		if w := getLineage(srv2, "ok.sql"); w.Code != http.StatusOK {
			t.Errorf("sequential request %d: want 200, got %d", i, w.Code)
		}
	}
}

const leakyDetail = "exec /opt/secret-install/bin/dataguard-engine [analyze --sql-file=ok.sql]: exit status 1: stderr: thread panicked at /home/builder/src/lineage.rs"

func TestLineageErrorResponseHasNoInternalDetail(t *testing.T) {
	base := sqlFixture(t)
	core, logs := observer.New(zap.ErrorLevel)
	rr := &recordingRunner{err: errors.New(leakyDetail)}
	srv := New(&fakeStore{}, rr, nil, "", zap.New(core), WithSQLDir(base))

	w := getLineage(srv, "ok.sql")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	assertNoInternalDetail(t, w.Body.String())
	if !strings.Contains(logs.All()[0].ContextMap()["error"].(string), "/opt/secret-install") {
		t.Errorf("detail should be kept in the server log: %+v", logs.All())
	}
}

// errStore は全メソッドが内部パスを含むエラーを返す Storer。
type errStore struct{ fakeStore }

var errLeaky = errors.New("badger: open /var/lib/dataguard/db/000001.vlog: permission denied")

func (errStore) CountViolations() (int, error)              { return 0, errLeaky }
func (errStore) CountViolationsByTable(string) (int, error) { return 0, errLeaky }
func (errStore) LatestDiff(string) (*store.SchemaDiff, error) {
	return nil, errLeaky
}
func (errStore) ListDiffs() ([]store.SchemaDiff, error) { return nil, errLeaky }
func (errStore) ListViolationsPaged(int, int) ([]engine.Violation, error) {
	return nil, errLeaky
}

func TestOtherHandlersDoNotLeakErrorDetail(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	srv := New(&errStore{}, &recordingRunner{}, nil, "", zap.New(core))
	for _, path := range []string{
		"/api/violations",
		"/api/violations?table=t",
		"/api/schema-diff",
		"/api/schema-diff?table=t",
	} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, path, nil)
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s: want 500, got %d", path, w.Code)
		}
		assertNoInternalDetail(t, w.Body.String())
	}
	if logs.Len() != 4 {
		t.Errorf("each failure should be logged server-side, got %d entries", logs.Len())
	}
}

func assertNoInternalDetail(t *testing.T, body string) {
	t.Helper()
	for _, banned := range []string{"/opt/", "/home/", "/var/", "stderr", "exit status", "badger", "panicked"} {
		if strings.Contains(body, banned) {
			t.Errorf("response leaks internal detail %q: %s", banned, body)
		}
	}
}
