package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

const (
	defaultLimit = 100
	maxLimit     = 1000
)

// handleViolations は GET /api/violations を処理する。
// クエリパラメータ: ?table=xxx&limit=100&offset=0
// レスポンスヘッダ: X-Total-Count (フィルタ前の保存済み総件数)
func (s *Server) handleViolations(c *gin.Context) {
	limit, offset, err := parsePagination(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	table := c.Query("table")

	// table が空なら全件。BadgerDB のキーが `violation:<table>:<id>` 形式のため、
	// table 指定時も prefix scan + ページングだけで完結する（#99）。
	total, err := s.store.CountViolationsFor(table)
	if err != nil {
		s.internalError(c, "violations.count", err)
		return
	}
	c.Header("X-Total-Count", strconv.Itoa(total))

	violations, err := s.store.ListViolationsFor(table, limit, offset)
	if err != nil {
		s.internalError(c, "violations.list", err)
		return
	}
	c.JSON(http.StatusOK, violations)
}

func parsePagination(c *gin.Context) (limit, offset int, err error) {
	limit = defaultLimit
	offset = 0

	if l := c.Query("limit"); l != "" {
		limit, err = strconv.Atoi(l)
		if err != nil || limit < 0 {
			return 0, 0, fmt.Errorf("limit must be a non-negative integer")
		}
		if limit > maxLimit {
			return 0, 0, fmt.Errorf("limit must not exceed %d", maxLimit)
		}
	}
	if o := c.Query("offset"); o != "" {
		offset, err = strconv.Atoi(o)
		if err != nil || offset < 0 {
			return 0, 0, fmt.Errorf("offset must be a non-negative integer")
		}
	}
	return limit, offset, nil
}
