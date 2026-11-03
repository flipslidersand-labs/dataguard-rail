// Package model は store / server / pipeline / engine が共有するドメイン型を置く。
// 実行方式 (exec / gRPC) の詳細を持つ engine から独立させ、依存の向きを一方向に保つ。
package model

// Violation は check の出力 (Rust 側 data-model.md の Violation に一致)。
type Violation struct {
	ID         string `json:"id"`
	Rule       string `json:"rule"`
	Table      string `json:"table"`
	Row        int    `json:"row"`
	Column     string `json:"column"`
	Value      string `json:"value"`
	DetectedAt string `json:"detected_at"`
}
