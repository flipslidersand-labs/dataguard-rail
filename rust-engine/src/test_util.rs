//! テスト共通ヘルパー。

/// 内容を書き込んだ一時ファイルを返す。返り値が Drop されるとファイルは削除される
/// (assert 失敗時も含む)。呼び出し側は検証が終わるまで束縛を保持すること。
pub fn write_temp(suffix: &str, content: &str) -> tempfile::NamedTempFile {
    use std::io::Write;
    let mut f = tempfile::Builder::new().suffix(suffix).tempfile().unwrap();
    f.write_all(content.as_bytes()).unwrap();
    f.flush().unwrap();
    f
}
