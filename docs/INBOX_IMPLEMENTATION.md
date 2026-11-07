# inbox 実装の判断と検証範囲

承認済み設計の PR-1〜PR-6 を順に実装する。GitHub App の接続・インストールは今回の対象外とし、provider interface までを実装した。

## 設計を具体化した箇所

- 追加の設計変更により inbox GET を削除した。既存の GET /api/projects の各項目に inbox_count と inbox（source_url・status のみ）を含め、CLI status がその応答を表示する。

- 最低 CLI バージョンと plugin の版を0.1.0にそろえた。Bearer の API 要求を版ヘッダーで確認し、Web と MCP を除外する。既存の開発用 `dev` は許可する。リリースの bump は PR-6 に含める。
- 新しい put は待機中の同じ source_url を置き換える。古い項目を固定した実行が失敗した場合も、新しい待機項目があれば古い項目を戻さない。同じ URL の fetched と removed が次回に同時に入る矛盾を防ぐ。
- inbox の200件には固定中の項目も数える。同じ URL の待機項目の置き換えは件数を増やさないため、満杯でも受け付ける。公開容量超過時は removed だけを許可する。
- 1 MiB は本文だけでなく put の JSON 全体の上限。CLI はファイルを1件ずつ送り、成功した時点で receipt を保存する。MCP も同じ put の形と上限を使う。
- 既存の入力 tar 上限256 MiBを維持した。最大の inbox と Wiki を同時に入力できる保証はない。超過した実行は失敗させ、項目を残す。分割実行や資料の黙った切り捨ては追加しない。
- 公開容量は指定どおり Revision.StoredBytes を使い、既存の計算に含まれる maps も数える。出力の硬い上限は提出された wiki tar の圧縮後・展開後それぞれに適用する。maps の付加後にもう一度容量で公開を拒否しない。
- 生成対象の容量判定には、直前に公開を完了した時点の容量を使う。途中公開中は Revision.CompleteBytes にその値を保持し、公開完了時に消す。途中書き込みだけで容量を超えても残った inbox を再実行でき、以前の公開ですでに超過していた場合は復旧中も removed だけを固定する。Revision.StoredBytes と新しい put の受付は、復旧中も現在の容量を使う。
- 出力の (a)〜(e) は source_url の完全一致で検査する。URL の正規化やリダイレクト先への置換をしない。slug、原文の忠実な保持、引用、index の分類、log の100項目・秘密除去はプロンプト規則であり、サーバーは内容を検査しない。
- GitHub は provider が owner の installation とリポジトリ ID を解決する。要求を inbox のリポジトリと contents:read に絞り、500件ごとに分ける。ほかの権限が必要な取得は失敗として扱い、スコープを追加しない。今回のサーバーは provider 未設定なので GitHub の資格情報を渡さない。
- GitHub provider は、発行応答を失った場合や再起動後も attempt ID で失効できる契約とした。サーバーは発行前に片付け義務を保存する。provider 不在・失効失敗では片付け完了にせず、復旧後に再試行する。将来の実 provider はこの契約を実装する必要がある。
- Google は Start 直前に refresh し、実行中には更新しない。vault を片付けても、今後の実行に必要な owner の Google 接続・grant は失効させない。短い access token は期限で失効する。
- OpenAI の複数資格情報は runner 内に閉じ込め、fake HTTP API で検証した。従来の Kioku ホストだけに固定する network override を除き、運用者の環境設定を使う。コードで新しい allowlist を設けない。既存の環境テンプレートは運用者が取得先に合わせて変更する。
- 移行は旧 wiki/maps/sources に加えて旧ジョブの記録・tar・一時 inbox・content transaction を消し、旧ジョブの資格情報を無効にする。監査とアカウント設定は保持する。schema 2 を最後に書き、再実行で新方式のデータを消さない。旧 catalog schema 1／metadata.json の自動移行は削除した。
- macOS の Unix socket のパス長に合わせ、バックアップのテスト用ソケットには短い相対パスを使った。製品のバックアップ API は変えていない。
- source_url 検査の HTTP 応答テストを PR-5、source ページ名・slug 衝突の公開往復テストを PR-6 で補った。実装の依存順は変えていない。

## 検証

各コミット後に `go vet ./...`、`go test -race ./...`、`go build ./cmd/...` と CI の plugin shell／Python／ビルド済み CLI テストを実行する。GitHub、Google、managed-agent API は fake とローカル HTTP サーバーを使い、実ネットワークを使わない。移行テストは一時データだけを使う。

| 設計の条件 | 主なテスト |
| --- | --- |
| pending／fetched／removed、混合・サイズ・URL拒否、外側 status、置き換え・200件、orphan GC | server/inbox_test.go、cli/inbox_test.go、source/provenance_test.go |
| agent／MCP URL put、新 CLI コマンド・426 | server/inbox_test.go、server/mcp_tools_test.go、cli/project_commands_test.go |
| 入力固定、途中 put、失敗・retry、空 inbox、上限時の removed 選択 | server/inbox_generation_test.go、server/org_test.go |
| 出力 (a)〜(e)、容量、受理済みハッシュ、公開時の上限超過 | server/output_source_validation_test.go、server/agent_io_test.go、server/agent_guards_review_test.go、server/agent_storage_reserve_test.go |
| source_url による更新・slug の -2、原文を公開 | wiki/source_validation_test.go、server/source_pages_roundtrip_test.go、wiki/assemble_test.go |
| 途中公開・期限切れ・再起動・片付け | server/inbox_generation_test.go、server/agent_lifecycle_test.go、server/agent_recovery_test.go、server/agent_cleanup_review_test.go |
| owner の資格情報、接続なし・発行失敗、500件分割、失効再試行、exec、秘密の非露出 | server/source_credentials_test.go、runner/* のテスト、managedagents/client_test.go |
| 移行の保持範囲・ロック・再開・再実行 | store/migrate_inbox_test.go |

表のパスは `internal/` 以下。slug や取得失敗の意味的な期待値は fake が作る出力で確かめる。実モデルがプロンプトどおりに取得・編集する保証を、このテスト結果から主張しない。

## 今回行わない作業

push、PR 作成、リリース、実データの移行、運用者の network 設定、GitHub App の認可／インストール実装、実 Google／GitHub／managed-agent API と実モデルの試験は行わない。新しい metrics は追加しない。実環境の導入は [移行手順](INBOX_MIGRATION.md) と [運用文書](WORKERS.md) に従う。
