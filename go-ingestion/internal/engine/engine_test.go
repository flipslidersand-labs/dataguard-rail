package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fakeBin は指定した stdout / 終了コードを返す実行可能スクリプトを作る。
func fakeBin(t *testing.T, stdout string, exitCode int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-engine")
	script := "#!/bin/sh\ncat <<'JSON'\n" + stdout + "\nJSON\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

func TestCheckParsesViolations(t *testing.T) {
	json := `[{"id":"viol-1","rule":"positive_price","table":"products","row":2,"column":"sale_price","value":"-5","detected_at":"2026-08-17T00:00:00+00:00"}]`
	r := New(fakeBin(t, json, 0))
	vs, err := r.Check(context.Background(), "in.csv", "rules.yaml")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("want 1 violation, got %d", len(vs))
	}
	if vs[0].Rule != "positive_price" || vs[0].Row != 2 || vs[0].Value != "-5" {
		t.Errorf("violation mismatch: %+v", vs[0])
	}
}

func TestCheckEmpty(t *testing.T) {
	r := New(fakeBin(t, "[]", 0))
	vs, err := r.Check(context.Background(), "in.csv", "rules.yaml")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(vs) != 0 {
		t.Errorf("want 0 violations, got %d", len(vs))
	}
}

func TestCheckNonZeroExit(t *testing.T) {
	r := New(fakeBin(t, "boom", 1))
	if _, err := r.Check(context.Background(), "in.csv", "rules.yaml"); err == nil {
		t.Error("expected error on non-zero exit")
	}
}

func TestCheckBadJSON(t *testing.T) {
	r := New(fakeBin(t, "not json", 0))
	if _, err := r.Check(context.Background(), "in.csv", "rules.yaml"); err == nil {
		t.Error("expected error on invalid json")
	}
}

func TestNewDefaultsBin(t *testing.T) {
	if New("").Bin != DefaultBin {
		t.Errorf("New(\"\") should default to %q", DefaultBin)
	}
}

func TestCheckRejectsRelativeBin(t *testing.T) {
	// 相対パスの Bin は PATH ハイジャック防止のため実行前にエラーになること。
	r := &Runner{Bin: "dataguard-engine"}
	if _, err := r.Check(context.Background(), "in.csv", "rules.yaml"); err == nil {
		t.Error("expected error when Bin is not an absolute path")
	}
}

func TestAnalyzeRejectsRelativeBin(t *testing.T) {
	r := &Runner{Bin: "dataguard-engine"}
	if _, err := r.Analyze(context.Background(), "schema.sql"); err == nil {
		t.Error("expected error when Bin is not an absolute path")
	}
}

// argsEchoBin は受け取った引数と作業ディレクトリを JSON で出力する実行可能スクリプトを作る。
func argsEchoBin(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "echo-engine")
	script := "#!/bin/sh\nprintf '{\"argc\":%s,\"arg1\":\"%s\",\"arg2\":\"%s\",\"pwd\":\"%s\"}' \"$#\" \"$2\" \"$3\" \"$(pwd -P)\"\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAnalyzePassesPathAsSingleSqlFileArgInSQLDir(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := New(argsEchoBin(t))
	r.SQLDir = dir
	raw, err := r.Analyze(context.Background(), "queries/report.sql")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	var got struct {
		Arg1 string `json:"arg1"`
		Arg2 string `json:"arg2"`
		Pwd  string `json:"pwd"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unexpected output %s: %v", raw, err)
	}
	// 引数は analyze --sql-file=<path> --out - (旧 --sql は使わない)
	if got.Arg1 != "--sql-file=queries/report.sql" {
		t.Errorf("path must be passed as one --sql-file=<path> arg, got %q", got.Arg1)
	}
	if got.Pwd != dir {
		t.Errorf("subprocess dir = %q, want %q", got.Pwd, dir)
	}
}

func TestAnalyzeRejectsLeadingDashAndEmptyPath(t *testing.T) {
	r := New(argsEchoBin(t))
	for _, p := range []string{"", "-x.sql", "--out=evil.sql"} {
		if _, err := r.Analyze(context.Background(), p); err == nil {
			t.Errorf("Analyze(%q) should be rejected before exec", p)
		}
	}
}
