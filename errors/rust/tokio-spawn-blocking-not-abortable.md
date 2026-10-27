---
title: "tokio::task::spawn_blocking のタスクは実行中に強制中断できない"
tags: [tokio, spawn_blocking, cancellation, grpc]
severity: medium
date: "2026-09-14"
---

## 症状

gRPCハンドラ（analyze/check）が同期I/O・CPU処理を直接 async fn 内で実行しており、tokioワーカースレッドを占有してしまう問題（#138）を修正する際、「クライアント切断を検知して処理を中断する」完全なキャンセル対応を検討したが、素直には実装できなかった。

## 原因

`tokio::task::spawn_blocking` が返す `JoinHandle` は `.abort()` を呼んでも、タスクが既に実行を開始していれば中断されない。Rustのスレッドは安全に強制終了できないため、tokioはブロッキングクロージャの実行完了を待つしかない。`JoinHandle` をdropしてもタスクはデタッチされて裏で走り続けるだけで、CPU/IO消費は止まらない。

## 解決策

- `spawn_blocking` でワーカースレッドの占有だけは解消する（本来の目的は達成できる）。
- 完全なキャンセルの代わりに、`tokio::time::timeout` でサーバー側の上限時間を設け、クライアントが既に諦めた後も無期限にリソースを消費し続ける事態だけは防ぐ。
- 「本当の中断」が必要な場合は、ブロッキングクロージャ自身の内部でポーリング的にキャンセルフラグ（`AtomicBool`/`CancellationToken`）をチェックする協調的キャンセルを実装するしかない（今回はコストに見合わず見送った）。

## 予防

「tonicのFuture drop検知でabortする」ような設計を提案・実装依頼された場合、まず「spawn_blockingは強制中断できない」という前提をレビューで指摘する。abortが効くのは実行開始前のタスクのみ。
