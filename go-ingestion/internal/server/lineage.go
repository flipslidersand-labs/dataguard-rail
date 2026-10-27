package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
)

var lineageTracer = otel.Tracer("dataguard-rail/server")

var (
	// errInvalidSQLPath は入力パスが不正(形式違反・基準ディレクトリ外・存在しない等)であることを表す。
	// 原因を区別して返すと基準ディレクトリ外のファイルの有無が推測できるため、理由は一つにまとめる。
	errInvalidSQLPath = errors.New("invalid sql path")
	// errSQLDir は基準ディレクトリ自体が解決できない(サーバー側の設定不備)ことを表す。
	errSQLDir = errors.New("sql dir is not usable")
)

// validateSQLPath は入力パスの形式を検証する。
// 空・絶対パス・".." を含む・先頭が "-"・".sql" 以外の拡張子を拒否し、相対パスのみ許可する。
// 基準ディレクトリ配下かどうかの確認(シンボリックリンク解決を含む)は resolveSQLPath が行う。
func validateSQLPath(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) {
		return false
	}
	if strings.HasPrefix(p, "-") {
		return false
	}
	if strings.Contains(p, "..") {
		return false
	}
	if !strings.HasSuffix(p, ".sql") {
		return false
	}
	// filepath.Clean で正規化後も ".." が現れないこと・絶対パス化しないこと・先頭が "-" でないことを確認
	cleaned := filepath.Clean(p)
	return !strings.Contains(cleaned, "..") && !filepath.IsAbs(cleaned) && !strings.HasPrefix(cleaned, "-")
}

// resolveSQLPath は p を baseDir と結合し、シンボリックリンクを解決したうえで
// baseDir 配下の通常ファイル(.sql)であることを確認する。
// 戻り値は解決後の baseDir からの相対パス(シンボリックリンクを含まない)。
func resolveSQLPath(baseDir, p string) (string, error) {
	if !validateSQLPath(p) {
		return "", errInvalidSQLPath
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", errSQLDir
	}
	base, err := filepath.EvalSymlinks(absBase)
	if err != nil {
		return "", errSQLDir
	}
	real, err := filepath.EvalSymlinks(filepath.Join(base, p))
	if err != nil {
		return "", errInvalidSQLPath
	}
	rel, err := filepath.Rel(base, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errInvalidSQLPath
	}
	if !strings.HasSuffix(real, ".sql") || strings.HasPrefix(rel, "-") {
		return "", errInvalidSQLPath
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.Mode().IsRegular() {
		return "", errInvalidSQLPath
	}
	return rel, nil
}

// handleLineage は GET /api/lineage?sql=path/to/file.sql を処理する。
// sql は --sql-dir(基準ディレクトリ)配下の .sql ファイルに限る。
func (s *Server) handleLineage(c *gin.Context) {
	_, span := lineageTracer.Start(c.Request.Context(), "lineage.analyze")
	defer span.End()

	sqlPath := c.Query("sql")
	if sqlPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sql パラメータは必須です"})
		return
	}
	rel, err := resolveSQLPath(s.sqlDir, sqlPath)
	switch {
	case errors.Is(err, errSQLDir):
		s.internalError(c, "lineage.resolve", err)
		return
	case err != nil:
		c.JSON(http.StatusBadRequest, gin.H{"error": "無効な sql パスです"})
		return
	}
	span.SetAttributes(attribute.String("sql.path", rel))

	// サブプロセスの同時実行数を制限する。上限に達していたら待たずに 429 を返す。
	select {
	case s.lineageSem <- struct{}{}:
		defer func() { <-s.lineageSem }()
	default:
		c.Header("Retry-After", "1")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "リクエストが集中しています。しばらくしてから再試行してください"})
		return
	}

	raw, err := s.runner.Analyze(c.Request.Context(), rel)
	if err != nil {
		s.log.Error("lineage analyze failed", zap.String("sql", rel), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lineage の解析に失敗しました"})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
}
