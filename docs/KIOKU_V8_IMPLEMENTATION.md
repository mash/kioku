# Kioku v8 実装記録

2026-11-18。`kioku-v8-impl-plan.html` の S1〜S8 を実装した。
外部仕様 `kioku-auto-select-v8.html` と異なる箇所は、実装計画末尾の「外部仕様の修正」を優先した。
各段階を独立したコミットにし、対象テスト・build・`make check` を実行する。

## 計画からの調整と理由

- **S1:** 既存テストが使う `saveSession` は薄いラッパーとして残した。状態の書き込みと排他制御は `updateSession` に集約し、既存のテスト呼び出しを保った。
- **S3:** `queue add` の URL は `https://` に加え、タブ・CR・LF を含まないことを確認する。URL に区切り文字があると、TSV の3列・1行の形式を壊すため。
- **S4:** 手元コピーの有無は `syncProject` の前に確認する。既存の `syncWiki` が通信前に `wiki/` を作るため、失敗後に確認すると、初回取得に失敗した空ディレクトリまで手元コピーとして扱ってしまう。`syncProject` の抽出では、紐づけ失敗時に従来の `pull` が結果 JSON を出さない挙動も保った。
- **S6:** 古い `capture.lock` の削除と排他作成を、ディレクトリの flock でも直列化した。複数の起動が同時に古いロックを見たとき、片方が新しいロックを削除する競合を防ぐ。`captureUnlock` が bg_id の記録より先に完了した場合は、ロックを復活させず `capture.last` に id を残す。
- **S7:** 背景の `capture run` には `KIOKU_SERVER`・`CLAUDE_CODE_SESSION_ID`・`KIOKU_SESSION_PULL` を継承しない。既定 server では計画どおり `--server` を省くため、親の `KIOKU_SERVER` で接続先が変わることを防ぎ、capture の子に親の選択更新用の印を渡さない。
- **S8:** `capture.md` の変更箇所一覧は後半の Claude Code の文も指定しているが、「変えないもの」の N4 は「共通の取得規則」以降を変更しないよう求める。後半は元の本文をそのまま保持し、冒頭で Claude Code local mode の起動・一時ディレクトリ・キュー処理に適用する優先規則を示した。原資料の取得規則と送信前の確認は変えていない。
- **S8:** CLI の help に残っていた「Hooks only read state」も更新した。自動選択だけが状態を書く実装と矛盾する説明を残さないため。
- **S8 の確認:** 計画の検索式 `hook-candidate` は、維持する `hook-candidates.txt` も拾う。検索結果がその正当なテスト参照だけであることを確認し、削除対象の `hook-candidate.txt` は完全なファイル名で別途確認した。

I15 の「Kioku なしでプロンプトに namespace も trigger 語も無ければ親ディレクトリを探索しない」を優先した。したがって、この状態ではブランチ名だけの trigger 語では選び直さない。これは実装計画に従った動作であり、selection.md にも明記した。

## 検証と実施範囲

- 各段階の Go テスト、`make build`、`make check`。`make check` は Codex packaging の Python テスト、`go test -race ./...`、`go vet ./...` を含む。
- S5・S7・S8 は `python3 -m unittest discover -s plugin/tests`。S7 はさらに `make build && python3 tools/test_claude_plugin.py --binary bin/kioku --plugin plugin` で実バイナリを指定した。
- 背景 pull の結合テストはローカル HTTP サーバー、capture の結合テストは制限した PATH の偽 `claude` を使う。実際の Claude Code は起動しない。テストの HOME・状態・認証・キューは一時ディレクトリに隔離する。
- `capture.md` の保持対象、`codex-plugin/`、`internal/cli/sync.go`、`hook-compact.txt`、`hook-no-cli.txt` が基準コミット `b45d19f` と同じであることを確認する。新しいトップレベルの Go Test 関数は計画の5個だけとし、ほかは既存テストのケースを追加する。
- S9 の実環境 smoke test と S10 の trigger 語の移行は実施しない。実際のユーザー設定・保存先・状態ディレクトリは変更せず、push・tag・バージョン更新・release もしない。

実際の Claude Code での背景セッション起動、MCP を通じた原資料の取得・put、idle セッションの `claude rm` による削除は S9 の未実施範囲に残る。
