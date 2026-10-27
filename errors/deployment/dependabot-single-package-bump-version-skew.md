---
title: "Dependabotの単体パッケージ更新がOTel/tonicで姉妹パッケージとのバージョンスキューを起こしCI失敗する"
tags: [dependabot, opentelemetry, tonic, ci, go, rust]
severity: medium
date: "2026-09-14"
---

## 症状

`.github/dependabot.yml` にgomod/cargo ecosystemを追加した直後、自動生成された9件のPRのうち複数がCI失敗した。

- `go.opentelemetry.io/otel/metric` を1.28.0→1.46.0に単体で上げたPRで、`go.opentelemetry.io/otel/sdk/metric`（1.28.0のまま）がビルドエラー: `*int64Inst does not implement "otel/metric".Int64Counter (missing method Enabled)`
- `tonic-build` を単体で0.11.0→0.14.6に上げたPRで、`tonic`本体が0.11.0のままのため不整合の疑い

## 原因

Dependabotはデフォルトで依存関係を1パッケージ1PRで個別に更新する。OpenTelemetryのapi/sdkパッケージ群やtonic/tonic-build/prostのように、同一メジャーバージョンで足並みを揃える必要があるパッケージ群を別々のPRで更新すると、片方だけ新しいAPIを要求し他方が追従できず壊れる。

## 解決策

安全に確認できた3件（badger, petgraph, clap — 単独で完結するパッケージ）のみマージし、OTel/tonic系は保留した。恒久対応はissue化（dataguard-rail#159）: `dependabot.yml`に`groups:`を設定し、密結合パッケージ群を1つのPRにまとめて更新させる。

## 予防

新しいecosystem（gomod/cargo等）をdependabotに追加する際は、事前に「同じメジャーバージョンで足並みを揃える必要があるパッケージ群がないか」（OTel api/sdk、tonic/tonic-build/prost、等）を確認し、`groups:`設定を最初から入れておく。
