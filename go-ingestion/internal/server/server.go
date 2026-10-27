// Package server は gin ベースの REST API サーバーを提供する。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/flipslidersand-labs/dataguard-rail/internal/alert"
	"github.com/flipslidersand-labs/dataguard-rail/internal/engine"
	"github.com/flipslidersand-labs/dataguard-rail/internal/store"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Storer は violations / schema の読み書きを抽象化する。テストで差し替え可能。
type Storer interface {
	ListViolationsPaged(limit, offset int) ([]engine.Violation, error)
	ListViolationsByTablePaged(table string, limit, offset int) ([]engine.Violation, error)
	CountViolations() (int, error)
	CountViolationsByTable(table string) (int, error)
	LatestDiff(table string) (*store.SchemaDiff, error)
	ListDiffs() ([]store.SchemaDiff, error)
}

// Runner は Rust engine の呼び出しを抽象化する。
type Runner interface {
	Analyze(ctx context.Context, sqlPath string) (json.RawMessage, error)
}

// Server は HTTP サーバーの依存セットを保持する。
type Server struct {
	store    Storer
	runner   Runner
	notifier alert.Notifier
	engine   *gin.Engine
	apiKey   string
	log      *zap.Logger

	// sqlDir は /api/lineage が読む .sql ファイルの基準ディレクトリ(既定: カレントディレクトリ)。
	sqlDir string
	// lineageSem は lineage 解析のサブプロセス同時実行数を制限するセマフォ。
	lineageSem chan struct{}
}

// DefaultMaxLineageConcurrency は lineage 解析の同時実行数の既定上限。
const DefaultMaxLineageConcurrency = 4

// Option は Server の任意設定。
type Option func(*Server)

// WithSQLDir は /api/lineage が読む .sql ファイルの基準ディレクトリを設定する。
func WithSQLDir(dir string) Option {
	return func(s *Server) { s.sqlDir = dir }
}

// WithMaxLineageConcurrency は lineage 解析の同時実行数の上限を設定する(n < 1 は無視)。
func WithMaxLineageConcurrency(n int) Option {
	return func(s *Server) {
		if n >= 1 {
			s.lineageSem = make(chan struct{}, n)
		}
	}
}

// internalError は詳細(絶対パス・stderr 等を含みうる err)をサーバーのログにだけ出し、
// クライアントには固定文言の 500 を返す。
func (s *Server) internalError(c *gin.Context, op string, err error) {
	s.log.Error("request failed", zap.String("op", op), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "内部エラーが発生しました"})
}

// New はサーバーインスタンスを生成する。apiKey が空の場合、認証は無効化される
// (ローカル開発用途のみを想定。本番運用では必ず指定すること)。
func New(st Storer, runner Runner, notifier alert.Notifier, apiKey string, log *zap.Logger, opts ...Option) *Server {
	if notifier == nil {
		notifier = alert.NoopNotifier{}
	}
	if log == nil {
		log = zap.NewNop()
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	s := &Server{
		store: st, runner: runner, notifier: notifier, engine: r, apiKey: apiKey, log: log,
		sqlDir:     ".",
		lineageSem: make(chan struct{}, DefaultMaxLineageConcurrency),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.engine.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	var protected []gin.HandlerFunc
	if s.apiKey != "" {
		protected = append(protected, s.authMiddleware())
	}

	// ダッシュボードの HTML は静的でデータを含まない(データは認証付きの /api から取得する)。
	// ブラウザのページ遷移では Authorization ヘッダを付けられないため、HTML 自体は認証なしで
	// 配信し、画面上のキー入力欄から /api へ Bearer を付けてアクセスする。
	s.engine.GET("/", s.handleDashboard)

	v1 := s.engine.Group("/api", protected...)
	v1.GET("/violations", s.handleViolations)
	v1.GET("/lineage", s.handleLineage)
	v1.GET("/schema-diff", s.handleSchemaDiff)
}

// authMiddleware は Authorization: Bearer <apiKey> ヘッダを検証する。
// 一致しない・欠落している場合は 401 で中断する。
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

// newHTTPServer は addr 用の http.Server を返す。
// Slowloris 型のリソース枯渇を防ぐため、明示的にタイムアウトを設定する。
func (s *Server) newHTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.engine,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// Run は addr で平文 HTTP のリッスンを開始する。
func (s *Server) Run(addr string) error {
	return s.newHTTPServer(addr).ListenAndServe()
}

// RunTLS は addr で TLS(HTTPS)のリッスンを開始する。certFile / keyFile は PEM。
func (s *Server) RunTLS(addr, certFile, keyFile string) error {
	return s.newHTTPServer(addr).ListenAndServeTLS(certFile, keyFile)
}

// Handler はテスト用に gin.Engine を返す。
func (s *Server) Handler() http.Handler {
	return s.engine
}
