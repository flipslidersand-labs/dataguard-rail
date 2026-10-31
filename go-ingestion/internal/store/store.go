// Package store は violations を BadgerDB に永続化する (ADR-004)。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/flipslidersand/dataguard-rail/internal/engine"
	"go.uber.org/zap"
)

// keyPrefix は violation レコードの key プレフィックス (`violation:<table>:<id>`)。
const keyPrefix = "violation:"

// Store は BadgerDB のラッパ。
type Store struct {
	db *badger.DB
}

// Open は指定パスに BadgerDB を開く。ログは抑制する。
func Open(path string) (*Store, error) {
	opts := badger.DefaultOptions(path).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open badger %q: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close は DB を閉じる。
func (s *Store) Close() error {
	return s.db.Close()
}

// gcInterval は RunGC が db.RunValueLogGC を試みる間隔 (var なのでテストで上書き可能)。
var gcInterval = 10 * time.Minute

// gcDiscardRatio は RunValueLogGC に渡す破棄率の閾値 (BadgerDB公式ドキュメント推奨値)。
const gcDiscardRatio = 0.5

// RunGC は ctx がキャンセルされるまで gcInterval ごとに BadgerDB の
// value log GC (RunValueLogGC) を実行し続けるブロッキングループ。
// SaveViolations による同一キー上書きで無効化された古い value log
// セグメントを回収しないとディスク使用量が単調増加し続けるため、
// 長時間稼働する daemon モードから goroutine として起動することを想定する
// (単発の `ingest` 実行では不要)。
//
// BadgerDB の一般的な運用パターンに従い、GC 1回で複数セグメントが
// 回収可能な場合に備えて ErrNoRewrite が返るまで RunValueLogGC を
// 連続実行する。ErrNoRewrite (回収対象なし) は正常終了として無視する。
func (s *Store) RunGC(ctx context.Context, log *zap.Logger) {
	if log == nil {
		log = zap.NewNop()
	}
	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				err := s.db.RunValueLogGC(gcDiscardRatio)
				if err == nil {
					continue
				}
				if !errors.Is(err, badger.ErrNoRewrite) {
					log.Warn("badger value log gc failed", zap.Error(err))
				}
				break
			}
		}
	}
}

// key は violation の一意キーを組み立てる。
func key(v engine.Violation) []byte {
	return []byte(fmt.Sprintf("%s%s:%s", keyPrefix, v.Table, v.ID))
}

// txnBatchSize は 1 トランザクションあたりの最大 violation 件数。
// BadgerDB の ErrTxnTooBig を避けるために小さく保つ。
const txnBatchSize = 1000

// SaveViolations は violations をバッチトランザクションで保存する。
// txnBatchSize 件ごと、または ErrTxnTooBig 発生時に commit & 再開する。
// 空のトランザクションに対しても ErrTxnTooBig になる (= 1 件で上限を超える)
// レコードは再試行しても成功しないため、無限ループせずエラーを返す。
func (s *Store) SaveViolations(violations []engine.Violation) error {
	txn := s.db.NewTransaction(true)
	committed := 0
	pending := 0 // 現在の txn に積んだ件数
	for i, v := range violations {
		data, err := json.Marshal(v)
		if err != nil {
			txn.Discard()
			return fmt.Errorf("marshal violation %s: %w", v.ID, err)
		}

		err = txn.Set(key(v), data)
		if errors.Is(err, badger.ErrTxnTooBig) && pending > 0 {
			// 積み済み分を commit し、空の txn で同じレコードを 1 回だけ再試行する。
			if commitErr := txn.Commit(); commitErr != nil {
				return fmt.Errorf("commit batch at index %d: %w", committed, commitErr)
			}
			committed = i
			pending = 0
			txn = s.db.NewTransaction(true)
			err = txn.Set(key(v), data)
		}
		if err != nil {
			txn.Discard()
			return fmt.Errorf("set violation %s (%d bytes): %w", v.ID, len(data), err)
		}
		pending++

		// バッチサイズに達したら commit して新しいトランザクションを開始する。
		if (i+1)%txnBatchSize == 0 {
			if commitErr := txn.Commit(); commitErr != nil {
				return fmt.Errorf("commit batch at index %d: %w", i, commitErr)
			}
			committed = i + 1
			pending = 0
			txn = s.db.NewTransaction(true)
		}
	}
	return txn.Commit()
}

// countByPrefix は prefix に一致するキーの件数をキーオンリースキャンで返す。
func (s *Store) countByPrefix(prefix []byte) (int, error) {
	count := 0
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			count++
		}
		return nil
	})
	return count, err
}

// listPagedByPrefix は prefix に一致する violation を offset から最大 limit 件返す。
// limit <= 0 は制限なし。全件ロードせず prefix scan + skip/limit だけで完結する。
func (s *Store) listPagedByPrefix(prefix []byte, limit, offset int) ([]engine.Violation, error) {
	var out []engine.Violation
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		skipped := 0
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if skipped < offset {
				skipped++
				continue
			}
			if limit > 0 && len(out) >= limit {
				break
			}
			if err := it.Item().Value(func(val []byte) error {
				var v engine.Violation
				if err := json.Unmarshal(val, &v); err != nil {
					return err
				}
				out = append(out, v)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// tableKeyPrefix は table 単位の violation キープレフィックスを組み立てる
// (`violation:<table>:`)。key() が既に `violation:<table>:<id>` の形式で
// 保存しているため、table フィルタは追加インデックス無しで prefix scan だけで済む。
// 注意: table が空文字だと `violation::` となり、全件用の keyPrefix (`violation:`) とは別物。
func tableKeyPrefix(table string) []byte {
	return []byte(fmt.Sprintf("%s%s:", keyPrefix, table))
}

// CountViolations は保存済みの violation 件数をキーオンリースキャンで返す。
func (s *Store) CountViolations() (int, error) {
	count, err := s.countByPrefix([]byte(keyPrefix))
	if err != nil {
		return 0, fmt.Errorf("count violations: %w", err)
	}
	return count, nil
}

// CountViolationsByTable は指定 table の violation 件数をキーオンリースキャンで返す。
func (s *Store) CountViolationsByTable(table string) (int, error) {
	count, err := s.countByPrefix(tableKeyPrefix(table))
	if err != nil {
		return 0, fmt.Errorf("count violations by table: %w", err)
	}
	return count, nil
}

// ListViolationsByTablePaged は指定 table に絞った violation を offset から
// 最大 limit 件返す。limit <= 0 は制限なし。
func (s *Store) ListViolationsByTablePaged(table string, limit, offset int) ([]engine.Violation, error) {
	out, err := s.listPagedByPrefix(tableKeyPrefix(table), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list violations by table paged: %w", err)
	}
	return out, nil
}

// ListViolationsPaged は offset から最大 limit 件の violation を返す。
// limit <= 0 は制限なし。
func (s *Store) ListViolationsPaged(limit, offset int) ([]engine.Violation, error) {
	out, err := s.listPagedByPrefix([]byte(keyPrefix), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list violations paged: %w", err)
	}
	return out, nil
}
