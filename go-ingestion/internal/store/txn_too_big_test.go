package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/flipslidersand/dataguard-rail/internal/engine"
)

// maxBatchSize は MemTableSize=1MiB のときの badger の txn 上限 (15%)。
const maxBatchSize = 157286

// TestSaveViolationsSingleRecordExceedsTxnLimit は、1 件だけで BadgerDB の
// トランザクション上限 (ErrTxnTooBig) を超えるレコードを保存しても、
// 無限リトライせずエラーを返すことを確認する (#176)。
// 上限は MemTableSize から導出されるため、小さい MemTableSize で再現する。
func TestSaveViolationsSingleRecordExceedsTxnLimit(t *testing.T) {
	opts := badger.DefaultOptions(t.TempDir()).
		WithLogger(nil).
		WithMemTableSize(1 << 20).       // maxBatchSize = 15% = 157286
		WithValueThreshold(maxBatchSize) // Open が許す最大。inline value が上限ぎりぎりになる
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatalf("badger.Open: %v", err)
	}
	s := &Store{db: db}
	defer s.Close()

	// JSON 化後の value が threshold-1 (inline のまま) になるよう調整する。
	// value は上限未満だが key 分が加わるため、空の txn でも ErrTxnTooBig になる。
	huge := engine.Violation{ID: "huge", Rule: "r", Table: "t"}
	base, _ := json.Marshal(huge)
	huge.Value = strings.Repeat("x", maxBatchSize-1-len(base))

	// 前提: 空の txn に単独で Set しても ErrTxnTooBig になる (badger 側仕様の変化検知)。
	txn := db.NewTransaction(true)
	data, _ := json.Marshal(huge)
	if err := txn.Set(key(huge), data); !errors.Is(err, badger.ErrTxnTooBig) {
		t.Fatalf("precondition: want ErrTxnTooBig on empty txn, got %v", err)
	}
	txn.Discard()

	vs := []engine.Violation{
		{ID: "small-1", Rule: "r", Table: "t", Value: "ok"},
		huge,
		{ID: "small-2", Rule: "r", Table: "t", Value: "ok"},
	}

	done := make(chan error, 1)
	go func() { done <- s.SaveViolations(vs) }()
	select {
	case err := <-done:
		if !errors.Is(err, badger.ErrTxnTooBig) {
			t.Fatalf("want error wrapping ErrTxnTooBig, got %v", err)
		}
		if !strings.Contains(err.Error(), "huge") {
			t.Errorf("error should name the offending violation ID, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SaveViolations did not return (infinite retry loop)")
	}
}
