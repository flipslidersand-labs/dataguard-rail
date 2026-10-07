# OTel OTLP gRPC exporter は go 1.25 必須

## 症状

```
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.28.0
go: google.golang.org/genproto/googleapis/api requires go >= 1.25.0
```

## 原因

OTLP gRPC exporter が依存する `google.golang.org/genproto` の最新版が go 1.25 を要求する。

## 回避策

go 1.24 環境では stdout exporter のみ使用する。

```go
// stdout のみ (go 1.24 互換)
traceExp, _ := stdouttrace.New(stdouttrace.WithPrettyPrint())
metricExp, _ := stdoutmetric.New()
```

OTLP は go.mod の `go 1.25` 以降に上げてから追加する。

## 関連 PR

dataguard-rail #34 (Phase 6 OTel)
