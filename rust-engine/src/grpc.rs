use crate::{check, lineage};
use anyhow::Context;
use std::time::Duration;
use tonic::{Request, Response, Status};

/// go-ingestion 側の `DefaultRPCTimeout`（internal/engine/grpc_client.go）と揃える。
/// クライアントが deadline を付けない呼び出しでも、サーバー側でブロッキング処理が
/// 無期限に CPU/IO を占有し続けないよう上限を設ける。
const SERVER_SIDE_TIMEOUT: Duration = Duration::from_secs(5 * 60);

// tonic-build が proto から生成するモジュール
pub mod dataguard {
    #![allow(clippy::result_large_err)]
    tonic::include_proto!("dataguard");
}

use dataguard::data_guard_server::DataGuard;
use dataguard::{AnalyzeRequest, AnalyzeResponse, CheckRequest, CheckResponse};

/// パストラバーサル防止: ".." コンポーネントおよび許可されていない拡張子を拒否する。
/// `allowed_exts` はいずれか1つに（大小文字を無視して）一致すれば許可する。
#[allow(clippy::result_large_err)]
fn validate_path(path: &str, allowed_exts: &[&str]) -> Result<(), Status> {
    use std::path::{Component, Path};
    let p = Path::new(path);
    let has_parent_dir = p.components().any(|c| c == Component::ParentDir);
    let has_allowed_ext = p
        .extension()
        .map(|e| allowed_exts.iter().any(|ext| e.eq_ignore_ascii_case(ext)))
        .unwrap_or(false);
    if p.is_absolute() || has_parent_dir || !has_allowed_ext {
        return Err(Status::invalid_argument(format!("無効なパスです: {path}")));
    }
    Ok(())
}

/// 基準ディレクトリ配下に読み取り対象を限定する。
///
/// `validate_path`（相対パス・`..` 拒否・拡張子）を通した後、基準ディレクトリと結合して
/// `canonicalize`（シンボリックリンク解決）し、解決後のパスが基準ディレクトリの配下に
/// あることを確認する。範囲外・存在しないパスは拒否する。
/// `base` は canonicalize 済みであること。
#[allow(clippy::result_large_err)]
fn resolve_in_base(
    base: &std::path::Path,
    path: &str,
    allowed_exts: &[&str],
) -> Result<std::path::PathBuf, Status> {
    validate_path(path, allowed_exts)?;
    let resolved = base
        .join(path)
        .canonicalize()
        .map_err(|_| Status::invalid_argument(format!("無効なパスです: {path}")))?;
    if !resolved.starts_with(base) {
        return Err(Status::invalid_argument(format!("無効なパスです: {path}")));
    }
    Ok(resolved)
}

pub struct DataGuardService {
    /// canonicalize 済みの基準ディレクトリ。RPC が読むファイルはこの配下に限る。
    base_dir: std::path::PathBuf,
}

impl DataGuardService {
    /// `base_dir` を canonicalize して保持する。存在しない場合はエラー。
    pub fn new(base_dir: &std::path::Path) -> anyhow::Result<Self> {
        let base_dir = base_dir
            .canonicalize()
            .with_context(|| format!("invalid --base-dir: {}", base_dir.display()))?;
        Ok(Self { base_dir })
    }
}

#[tonic::async_trait]
impl DataGuard for DataGuardService {
    async fn analyze(
        &self,
        request: Request<AnalyzeRequest>,
    ) -> Result<Response<AnalyzeResponse>, Status> {
        let sql_path = request.into_inner().sql_path;
        let sql_resolved = resolve_in_base(&self.base_dir, &sql_path, &["sql"])?;

        // ファイル読み込み・sqlparser解析は同期処理のため、tokioワーカースレッドを
        // 占有しないよう spawn_blocking の専用スレッドプールへ逃がす（#138）。
        #[allow(clippy::result_large_err)]
        let handle = tokio::task::spawn_blocking(move || -> Result<String, Status> {
            let sql_text = std::fs::read_to_string(&sql_resolved)
                .with_context(|| format!("cannot read {sql_path}"))
                .map_err(|e| Status::invalid_argument(e.to_string()))?;

            let report =
                lineage::analyze(&sql_text).map_err(|e| Status::internal(e.to_string()))?;

            serde_json::to_string(&report).map_err(|e| Status::internal(e.to_string()))
        });

        // spawn_blocking の完了は待ち続けても中断できないため（tokioはブロッキング
        // スレッドを強制終了できない）、サーバー側にも上限時間を設けクライアントの
        // タイムアウト後にCPU/IOを無期限消費し続けるのを防ぐ。
        match tokio::time::timeout(SERVER_SIDE_TIMEOUT, handle).await {
            Ok(Ok(Ok(lineage_json))) => Ok(Response::new(AnalyzeResponse { lineage_json })),
            Ok(Ok(Err(status))) => Err(status),
            Ok(Err(join_err)) => Err(Status::internal(format!(
                "analyze の内部タスクが異常終了しました: {join_err}"
            ))),
            Err(_elapsed) => Err(Status::deadline_exceeded(format!(
                "analyze が {SERVER_SIDE_TIMEOUT:?} 以内に完了しませんでした"
            ))),
        }
    }

    async fn check(
        &self,
        request: Request<CheckRequest>,
    ) -> Result<Response<CheckResponse>, Status> {
        let req = request.into_inner();
        let csv_resolved = resolve_in_base(&self.base_dir, &req.csv_path, &["csv"])?;
        let rules_resolved = resolve_in_base(&self.base_dir, &req.rules_path, &["yaml", "yml"])?;
        let detected_at = chrono::Utc::now().to_rfc3339();

        // check_file は最大10万行の同期パース・正規表現評価・集計を行うため、
        // analyze と同様に spawn_blocking + サーバー側タイムアウトで保護する（#138）。
        #[allow(clippy::result_large_err)]
        let handle = tokio::task::spawn_blocking(move || -> Result<String, Status> {
            let violations = check::check_file(
                &csv_resolved.to_string_lossy(),
                &rules_resolved.to_string_lossy(),
                &detected_at,
            )
            .map_err(|e| Status::internal(e.to_string()))?;

            serde_json::to_string(&violations).map_err(|e| Status::internal(e.to_string()))
        });

        match tokio::time::timeout(SERVER_SIDE_TIMEOUT, handle).await {
            Ok(Ok(Ok(violations_json))) => Ok(Response::new(CheckResponse { violations_json })),
            Ok(Ok(Err(status))) => Err(status),
            Ok(Err(join_err)) => Err(Status::internal(format!(
                "check の内部タスクが異常終了しました: {join_err}"
            ))),
            Err(_elapsed) => Err(Status::deadline_exceeded(format!(
                "check が {SERVER_SIDE_TIMEOUT:?} 以内に完了しませんでした"
            ))),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validate_path_accepts_matching_extension() {
        assert!(validate_path("data/input.csv", &["csv"]).is_ok());
        assert!(validate_path("rules.YAML", &["yaml", "yml"]).is_ok());
        assert!(validate_path("rules.yml", &["yaml", "yml"]).is_ok());
    }

    #[test]
    fn validate_path_rejects_parent_dir_traversal() {
        assert!(validate_path("../../etc/passwd.csv", &["csv"]).is_err());
        assert!(validate_path("data/../../secret.sql", &["sql"]).is_err());
    }

    #[test]
    fn validate_path_rejects_absolute_path() {
        assert!(validate_path("/etc/passwd.csv", &["csv"]).is_err());
        assert!(validate_path("/etc/passwd.sql", &["sql"]).is_err());
        assert!(validate_path("/etc/passwd.yaml", &["yaml", "yml"]).is_err());
    }

    #[test]
    fn validate_path_rejects_wrong_extension() {
        assert!(validate_path("data/input.txt", &["csv"]).is_err());
        assert!(validate_path("rules.json", &["yaml", "yml"]).is_err());
        assert!(validate_path("no_extension", &["csv"]).is_err());
    }

    fn base_with_files() -> (tempfile::TempDir, tempfile::TempDir) {
        let base = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        std::fs::create_dir(base.path().join("sub")).unwrap();
        std::fs::write(base.path().join("sub/ok.sql"), "SELECT 1").unwrap();
        std::fs::write(outside.path().join("secret.sql"), "SELECT 2").unwrap();
        (base, outside)
    }

    #[test]
    fn resolve_in_base_accepts_file_under_base() {
        let (base, _outside) = base_with_files();
        let base_canon = base.path().canonicalize().unwrap();
        let got = resolve_in_base(&base_canon, "sub/ok.sql", &["sql"]).unwrap();
        assert!(got.starts_with(&base_canon));
    }

    #[test]
    fn resolve_in_base_rejects_missing_file() {
        let (base, _outside) = base_with_files();
        let base_canon = base.path().canonicalize().unwrap();
        assert!(resolve_in_base(&base_canon, "sub/none.sql", &["sql"]).is_err());
    }

    #[cfg(unix)]
    #[test]
    fn resolve_in_base_rejects_symlink_file_escaping_base() {
        let (base, outside) = base_with_files();
        let base_canon = base.path().canonicalize().unwrap();
        std::os::unix::fs::symlink(
            outside.path().join("secret.sql"),
            base.path().join("link.sql"),
        )
        .unwrap();
        assert!(resolve_in_base(&base_canon, "link.sql", &["sql"]).is_err());
    }

    #[cfg(unix)]
    #[test]
    fn resolve_in_base_rejects_symlinked_dir_escaping_base() {
        let (base, outside) = base_with_files();
        let base_canon = base.path().canonicalize().unwrap();
        std::os::unix::fs::symlink(outside.path(), base.path().join("linkdir")).unwrap();
        assert!(resolve_in_base(&base_canon, "linkdir/secret.sql", &["sql"]).is_err());
    }

    #[cfg(unix)]
    #[test]
    fn resolve_in_base_allows_symlink_that_stays_inside_base() {
        let (base, _outside) = base_with_files();
        let base_canon = base.path().canonicalize().unwrap();
        std::os::unix::fs::symlink(
            base.path().join("sub/ok.sql"),
            base.path().join("alias.sql"),
        )
        .unwrap();
        assert!(resolve_in_base(&base_canon, "alias.sql", &["sql"]).is_ok());
    }

    #[test]
    fn resolve_in_base_keeps_absolute_and_parent_dir_rejection() {
        let (base, outside) = base_with_files();
        let base_canon = base.path().canonicalize().unwrap();
        let abs = outside.path().join("secret.sql");
        assert!(resolve_in_base(&base_canon, abs.to_str().unwrap(), &["sql"]).is_err());
        assert!(resolve_in_base(&base_canon, "sub/../sub/ok.sql", &["sql"]).is_err());
    }

    #[test]
    fn service_new_rejects_missing_base_dir() {
        assert!(DataGuardService::new(std::path::Path::new("/nonexistent-base-dir-xyz")).is_err());
    }

    /// 一時ディレクトリを基準ディレクトリ（canonicalize 済み）としたサービスを作る。
    fn svc_with_base() -> (tempfile::TempDir, DataGuardService) {
        let dir = tempfile::tempdir().unwrap();
        let svc = DataGuardService::new(&dir.path().canonicalize().unwrap()).unwrap();
        (dir, svc)
    }

    /// 基準ディレクトリ直下にファイルを書き、基準からの相対パスを返す。
    fn write_rel(dir: &tempfile::TempDir, name: &str, content: &str) -> String {
        std::fs::write(dir.path().join(name), content).unwrap();
        name.to_string()
    }

    #[tokio::test]
    async fn analyze_returns_lineage_json_for_valid_sql() {
        let (dir, svc) = svc_with_base();
        let sql_path = write_rel(&dir, "q.sql", "INSERT INTO dst SELECT id FROM src;");
        let resp = svc
            .analyze(Request::new(AnalyzeRequest { sql_path }))
            .await
            .expect("analyze should succeed");
        let v: serde_json::Value = serde_json::from_str(&resp.into_inner().lineage_json).unwrap();
        let tables: Vec<&str> = v["tables"]
            .as_array()
            .unwrap()
            .iter()
            .map(|t| t.as_str().unwrap())
            .collect();
        assert!(tables.contains(&"src") && tables.contains(&"dst"), "{v}");
        assert!(!v["edges"].as_array().unwrap().is_empty(), "{v}");
    }

    #[tokio::test]
    async fn analyze_missing_file_is_invalid_argument() {
        let (_dir, svc) = svc_with_base();
        let err = svc
            .analyze(Request::new(AnalyzeRequest {
                sql_path: "missing.sql".into(),
            }))
            .await
            .expect_err("missing file should fail");
        assert_eq!(err.code(), tonic::Code::InvalidArgument);
    }

    #[tokio::test]
    async fn analyze_invalid_sql_is_internal() {
        let (dir, svc) = svc_with_base();
        let sql_path = write_rel(&dir, "bad.sql", "SELEC FROM WHERE ;;(");
        let err = svc
            .analyze(Request::new(AnalyzeRequest { sql_path }))
            .await
            .expect_err("invalid SQL should fail");
        assert_eq!(err.code(), tonic::Code::Internal);
    }

    #[tokio::test]
    async fn check_returns_violations_json_for_valid_inputs() {
        let (dir, svc) = svc_with_base();
        let csv_path = write_rel(&dir, "d.csv", "sale_price\n100\n-5\n");
        let rules_path = write_rel(
            &dir,
            "r.yaml",
            "rules:\n  - name: positive_price\n    column: sale_price\n    expression: \"value > 0\"\n",
        );
        let resp = svc
            .check(Request::new(CheckRequest {
                csv_path,
                rules_path,
            }))
            .await
            .expect("check should succeed");
        let v: serde_json::Value =
            serde_json::from_str(&resp.into_inner().violations_json).unwrap();
        let arr = v.as_array().unwrap();
        assert_eq!(arr.len(), 1, "{v}");
        assert_eq!(arr[0]["rule"], "positive_price");
        assert_eq!(arr[0]["value"], "-5");
    }

    #[tokio::test]
    async fn check_with_invalid_rule_is_internal() {
        let (dir, svc) = svc_with_base();
        let csv_path = write_rel(&dir, "d.csv", "x\n1\n");
        let rules_path = write_rel(
            &dir,
            "r.yaml",
            "rules:\n  - name: bad\n    column: x\n    expression: \"value ~= 1\"\n",
        );
        let err = svc
            .check(Request::new(CheckRequest {
                csv_path,
                rules_path,
            }))
            .await
            .expect_err("invalid rule should fail");
        assert_eq!(err.code(), tonic::Code::Internal);
    }

    #[tokio::test]
    async fn check_rejects_invalid_paths_before_io() {
        let (_dir, svc) = svc_with_base();
        let err = svc
            .check(Request::new(CheckRequest {
                csv_path: "../x.csv".into(),
                rules_path: "r.yaml".into(),
            }))
            .await
            .expect_err("traversal should be rejected");
        assert_eq!(err.code(), tonic::Code::InvalidArgument);
    }
}
