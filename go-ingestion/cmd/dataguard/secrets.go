package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/flipslidersand-labs/dataguard-rail/internal/engine"
	"go.uber.org/zap"
)

const (
	envAPIKey       = "DATAGUARD_API_KEY"
	envSlackWebhook = "DATAGUARD_SLACK_WEBHOOK"
	// envAllowInsecure は無認証起動・ループバック外の平文公開を明示的に許可する opt-in。
	envAllowInsecure = "DATAGUARD_ALLOW_INSECURE"
)

// resolveSecret は秘密値を「ファイル」「コマンドライン引数」「環境変数」から解決する。
//
//   - argVal と filePath の同時指定はエラー(どちらが有効か曖昧になるため)。
//   - どちらも無ければ envVal(環境変数の値)を使う。
//   - 引数(argVal)が使われた場合は、プロセス一覧に値が見える旨の警告をログに出す。
//     値そのものはログに出さない。
//   - ファイルは前後の空白・改行を除去し、空ならエラーにする。
func resolveSecret(log *zap.Logger, flagName, argVal, filePath, envVal string) (string, error) {
	if argVal != "" && filePath != "" {
		return "", fmt.Errorf("--%s と --%s-file は同時に指定できません", flagName, flagName)
	}
	if filePath != "" {
		b, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("--%s-file を読めません: %w", flagName, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("--%s-file が空です: %s", flagName, filePath)
		}
		return v, nil
	}
	if argVal != "" {
		log.Warn("コマンドライン引数で渡した値はプロセス一覧 (ps, /proc/<pid>/cmdline) から見えます。"+
			"--"+flagName+"-file か環境変数での指定を推奨します",
			zap.String("flag", "--"+flagName))
		return argVal, nil
	}
	return envVal, nil
}

// checkListenPolicy は REST のバインド先に応じた TLS 要件を判定する。
//
//   - --tls-cert と --tls-key は併用必須(片方だけはエラー)。
//   - ループバックなら TLS なしでよい。
//   - ループバック外で TLS が無い場合は、allowInsecureEnv が "1" のときだけ許可する。
//
// ループバックの判定は gRPC クライアントと同じ engine.IsLoopbackAddr を使う。
func checkListenPolicy(addr, tlsCert, tlsKey, allowInsecureEnv string) error {
	if (tlsCert == "") != (tlsKey == "") {
		return fmt.Errorf("--tls-cert と --tls-key は併用してください")
	}
	if tlsCert != "" || engine.IsLoopbackAddr(addr) {
		return nil
	}
	if allowInsecureEnv != "1" {
		return fmt.Errorf(
			"--addr %q はループバック外ですが TLS がありません。--tls-cert/--tls-key を指定するか、"+
				"平文公開を意図する場合のみ環境変数 %s=1 を設定してください", addr, envAllowInsecure)
	}
	return nil
}
