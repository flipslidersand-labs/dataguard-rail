package engine

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/flipslidersand/dataguard-rail/internal/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeEngine は pb.DataGuardServer のテスト用実装。受信した値と ctx の deadline を記録する。
type fakeEngine struct {
	pb.UnimplementedDataGuardServer

	analyzeResp *pb.AnalyzeResponse
	analyzeErr  error
	checkResp   *pb.CheckResponse
	checkErr    error

	gotSQLPath  string
	gotCSVPath  string
	gotRules    string
	hadDeadline bool
	deadlineIn  time.Duration
}

func (f *fakeEngine) record(ctx context.Context) {
	if d, ok := ctx.Deadline(); ok {
		f.hadDeadline = true
		f.deadlineIn = time.Until(d)
	}
}

func (f *fakeEngine) Analyze(ctx context.Context, r *pb.AnalyzeRequest) (*pb.AnalyzeResponse, error) {
	f.record(ctx)
	f.gotSQLPath = r.SqlPath
	return f.analyzeResp, f.analyzeErr
}

func (f *fakeEngine) Check(ctx context.Context, r *pb.CheckRequest) (*pb.CheckResponse, error) {
	f.record(ctx)
	f.gotCSVPath, f.gotRules = r.CsvPath, r.RulesPath
	return f.checkResp, f.checkErr
}

// newBufconnRunner は in-process (bufconn) の gRPC サーバー上に fake を立て、実ネットワークを使わずに
// GrpcRunner を返す。
func newBufconnRunner(t *testing.T, fake *fakeEngine) *GrpcRunner {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterDataGuardServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &GrpcRunner{conn: conn, client: pb.NewDataGuardClient(conn)}
}

func TestGrpcRunnerAnalyzeSuccess(t *testing.T) {
	fake := &fakeEngine{analyzeResp: &pb.AnalyzeResponse{LineageJson: `{"nodes":[1,2]}`}}
	r := newBufconnRunner(t, fake)

	got, err := r.Analyze(context.Background(), "q.sql")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if string(got) != `{"nodes":[1,2]}` {
		t.Errorf("lineage = %s, want verbatim lineage_json", got)
	}
	if fake.gotSQLPath != "q.sql" {
		t.Errorf("server got sql_path %q, want q.sql", fake.gotSQLPath)
	}
}

func TestGrpcRunnerAnalyzeRPCError(t *testing.T) {
	fake := &fakeEngine{analyzeErr: status.Error(codes.InvalidArgument, "bad sql")}
	r := newBufconnRunner(t, fake)

	got, err := r.Analyze(context.Background(), "q.sql")
	if err == nil {
		t.Fatalf("want error, got result %s", got)
	}
	if got != nil {
		t.Errorf("result on error = %s, want nil", got)
	}
	if !strings.HasPrefix(err.Error(), "grpc Analyze: ") {
		t.Errorf("error %q lacks the \"grpc Analyze: \" wrapper prefix", err)
	}
	// %w で包んでいるので gRPC の status (code) が取り出せ、サーバーのメッセージが保たれる。
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument || !strings.Contains(err.Error(), "bad sql") {
		t.Errorf("status = (%v, ok=%v), want InvalidArgument carrying \"bad sql\"", st, ok)
	}
}

func TestGrpcRunnerCheckSuccess(t *testing.T) {
	fake := &fakeEngine{checkResp: &pb.CheckResponse{ViolationsJson: `[` +
		`{"id":"v1","rule":"positive_price","table":"products","row":2,"column":"sale_price","value":"-5","detected_at":"2026-10-31T00:00:00Z"}]`}}
	r := newBufconnRunner(t, fake)

	got, err := r.Check(context.Background(), "in.csv", "rules.yaml")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := Violation{ID: "v1", Rule: "positive_price", Table: "products", Row: 2,
		Column: "sale_price", Value: "-5", DetectedAt: "2026-10-31T00:00:00Z"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("violations = %+v, want [%+v]", got, want)
	}
	if fake.gotCSVPath != "in.csv" || fake.gotRules != "rules.yaml" {
		t.Errorf("server got (%q, %q), want (in.csv, rules.yaml)", fake.gotCSVPath, fake.gotRules)
	}
}

func TestGrpcRunnerCheckEmptyViolations(t *testing.T) {
	r := newBufconnRunner(t, &fakeEngine{checkResp: &pb.CheckResponse{ViolationsJson: `[]`}})
	got, err := r.Check(context.Background(), "in.csv", "rules.yaml")
	if err != nil || len(got) != 0 {
		t.Errorf("Check = (%v, %v), want empty and no error", got, err)
	}
}

func TestGrpcRunnerCheckRPCError(t *testing.T) {
	fake := &fakeEngine{checkErr: status.Error(codes.NotFound, "rules missing")}
	r := newBufconnRunner(t, fake)

	got, err := r.Check(context.Background(), "in.csv", "rules.yaml")
	if err == nil || got != nil {
		t.Fatalf("Check = (%v, %v), want (nil, error)", got, err)
	}
	if !strings.HasPrefix(err.Error(), "grpc Check: ") {
		t.Errorf("error %q lacks the \"grpc Check: \" wrapper prefix", err)
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		t.Errorf("status = (%v, ok=%v), want NotFound", st, ok)
	}
}

// Rust エンジンが不正な violations_json を返した場合: 成功扱いにせず、unmarshal エラーとして返す。
// エラー文字列に受信ペイロード (CSV 値など機微になりうる内容) が含まれないことも確認する。
func TestGrpcRunnerCheckInvalidViolationsJSON(t *testing.T) {
	const secret = "SECRET-CELL-VALUE"
	for name, payload := range map[string]string{
		"not json":    `{"value":"` + secret + `" this is not json`,
		"wrong shape": `{"value":"` + secret + `"}`, // 配列ではなくオブジェクト
	} {
		t.Run(name, func(t *testing.T) {
			r := newBufconnRunner(t, &fakeEngine{checkResp: &pb.CheckResponse{ViolationsJson: payload}})

			got, err := r.Check(context.Background(), "in.csv", "rules.yaml")
			if err == nil {
				t.Fatalf("want error, got violations %v", got)
			}
			if got != nil {
				t.Errorf("violations on error = %v, want nil", got)
			}
			if !strings.HasPrefix(err.Error(), "unmarshal violations: ") {
				t.Errorf("error %q lacks the \"unmarshal violations: \" wrapper prefix", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks the response payload: %q", err)
			}
		})
	}
}

// サーバーに繋がらない場合は Unavailable が %w で伝わり、Analyze/Check とも結果を返さない。
func TestGrpcRunnerServerUnavailable(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterDataGuardServer(srv, &fakeEngine{})
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	r := &GrpcRunner{conn: conn, client: pb.NewDataGuardClient(conn)}
	srv.Stop() // 以後の接続は拒否される

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.Analyze(ctx, "q.sql"); err == nil {
		t.Error("Analyze: want error")
	} else if st, _ := status.FromError(err); st.Code() != codes.Unavailable {
		t.Errorf("Analyze code = %v, want Unavailable (err=%v)", st.Code(), err)
	}
	if _, err := r.Check(ctx, "in.csv", "rules.yaml"); err == nil {
		t.Error("Check: want error")
	} else if st, _ := status.FromError(err); st.Code() != codes.Unavailable {
		t.Errorf("Check code = %v, want Unavailable (err=%v)", st.Code(), err)
	}
}

// deadline 無しの ctx には DefaultRPCTimeout が付き、呼び出し元の deadline はそのまま尊重される。
func TestGrpcRunnerAppliesDefaultTimeout(t *testing.T) {
	fake := &fakeEngine{
		analyzeResp: &pb.AnalyzeResponse{LineageJson: `{}`},
		checkResp:   &pb.CheckResponse{ViolationsJson: `[]`},
	}
	r := newBufconnRunner(t, fake)

	if _, err := r.Analyze(context.Background(), "q.sql"); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !fake.hadDeadline || fake.deadlineIn <= DefaultRPCTimeout-time.Minute || fake.deadlineIn > DefaultRPCTimeout {
		t.Errorf("Analyze: server saw deadline=%v in %v, want ~DefaultRPCTimeout", fake.hadDeadline, fake.deadlineIn)
	}

	fake.hadDeadline = false
	if _, err := r.Check(context.Background(), "in.csv", "rules.yaml"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !fake.hadDeadline || fake.deadlineIn <= DefaultRPCTimeout-time.Minute || fake.deadlineIn > DefaultRPCTimeout {
		t.Errorf("Check: server saw deadline=%v in %v, want ~DefaultRPCTimeout", fake.hadDeadline, fake.deadlineIn)
	}

	// 呼び出し元が短い deadline を指定したら、それが優先される (延長されない)。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fake.hadDeadline = false
	if _, err := r.Check(ctx, "in.csv", "rules.yaml"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !fake.hadDeadline || fake.deadlineIn > 30*time.Second {
		t.Errorf("caller deadline not respected: saw deadline=%v in %v, want <= 30s", fake.hadDeadline, fake.deadlineIn)
	}
}

// errors パッケージ経由でも status が辿れること (%w の維持)。
func TestGrpcRunnerErrorsAreWrapped(t *testing.T) {
	r := newBufconnRunner(t, &fakeEngine{checkErr: status.Error(codes.Internal, "x")})
	_, err := r.Check(context.Background(), "in.csv", "rules.yaml")
	var se interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &se) || se.GRPCStatus().Code() != codes.Internal {
		t.Errorf("errors.As GRPCStatus failed or wrong code: %v", err)
	}
}
