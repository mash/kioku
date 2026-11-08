# inbox 実装の判断と検証範囲

inbox はクライアントが送った本文と削除の依頼を保存する。削除の依頼だけに `remove: true` を付ける。ユーザーの Claude が資料を取得し、本文と source_url を送る。Kioku は資料取得用の資格情報を保持しない。

## 設計を具体化した箇所

- 追加の設計変更により inbox GET を削除した。既存の GET /api/projects の各項目に inbox_count と inbox（source_url と削除の依頼だけに付ける remove: true）を含め、CLI status がその応答を表示する。

- 最低 CLI バージョンは0.1.0。Bearer の API 要求を版ヘッダーで確認し、Web と MCP を除外する。開発用の `dev` は許可する。
- 新しい put は待機中の同じ source_url を置き換える。古い項目を固定した実行が失敗した場合も、新しい待機項目があれば古い項目を戻さない。同じ URL の本文と削除の依頼 が次回に同時に入る矛盾を防ぐ。
- inbox の200件には固定中の項目も数える。同じ URL の待機項目の置き換えは件数を増やさないため、満杯でも受け付ける。公開容量超過時は 削除の依頼だけを許可する。
- 1 MiB は本文だけでなく put の JSON 全体の上限。CLI は指定したファイルを1件ずつ送り、成功した時点で `accepted <path>` を表示する。MCP も同じ put の形と上限を使う。
- 既存の入力 tar 上限256 MiBを維持した。最大の inbox と Wiki を同時に入力できる保証はない。超過した実行は失敗させ、項目を残す。分割実行や資料の黙った切り捨ては追加しない。
- 公開容量は指定どおり Revision.StoredBytes を使い、既存の計算に含まれる maps も数える。出力の硬い上限は提出された wiki tar の圧縮後・展開後それぞれに適用する。maps の付加後にもう一度容量で公開を拒否しない。
- 生成対象の容量判定には、直前に公開を完了した時点の容量を使う。途中公開中は Revision.CompleteBytes にその値を保持し、公開完了時に消す。途中書き込みだけで容量を超えても残った inbox を再実行でき、以前の公開ですでに超過していた場合は復旧中も 削除の依頼だけを固定する。Revision.StoredBytes と新しい put の受付は、復旧中も現在の容量を使う。
- 出力の (a)〜(e) は source_url の完全一致で検査する。URL の正規化やリダイレクト先への置換をしない。slug、原文の忠実な保持、引用、index の分類、log の100項目・秘密除去はプロンプト規則であり、サーバーは内容を検査しない。
- source_url はクライアントが申告する出典である。サーバーは URL の形式を検査するが、本文と原本の一致や投稿者の原本への閲覧権限を検証しない。
- runner は remove フラグで本文と削除の依頼を区別して処理する。managed runner の入出力はプロバイダーのファイルAPIで受け渡す。Google／GitHub の OAuth はログインに使う。
- Claudeの環境テンプレートでは allowed_hosts を空にする。運用者が環境を用意し、kiokud が起動前のprovider API照合で通信制限を確認する。OpenAIはセッション作成時に通信を無効にする。
- macOS の Unix socket のパス長に合わせ、バックアップのテスト用ソケットには短い相対パスを使った。製品のバックアップ API は変えていない。
- source_url 検査の HTTP 応答テストを PR-5、source ページ名・slug 衝突の公開往復テストを PR-6 で補った。実装の依存順は変えていない。

## 検証

`go build ./...` と `make check`（`go test -race ./...`、`go vet ./...`）で検証する。GitHub／Google のログインと managed-agent API は fake とローカル HTTP サーバーを使う。

| 設計の条件 | 主なテスト |
| --- | --- |
| 本文／削除の依頼、混合・サイズ・URL拒否、外側 remove、置き換え・200件、orphan GC | server/inbox_test.go、cli/inbox_test.go、source/provenance_test.go |
| agent／MCP 本文 put、新 CLI コマンド・426 | server/inbox_test.go、server/mcp_tools_test.go、cli/project_commands_test.go |
| 入力固定、途中 put、失敗・retry、空 inbox、上限時の 削除の依頼の選択 | server/inbox_generation_test.go、server/org_test.go |
| 出力 (a)〜(e)、容量、受理済みハッシュ、公開時の上限超過 | server/output_source_validation_test.go、server/agent_io_test.go、server/agent_guards_review_test.go、server/agent_storage_reserve_test.go |
| source_url による更新・slug の -2、原文を公開 | wiki/source_validation_test.go、server/source_pages_roundtrip_test.go、wiki/assemble_test.go |
| 途中公開・期限切れ・再起動・片付け | server/inbox_generation_test.go、server/agent_lifecycle_test.go、server/agent_recovery_test.go、server/agent_cleanup_review_test.go |
| managed runner のファイル入出力と通信遮断、自前の exec とジョブトークン | runner/* のテスト、agentgen/config_test.go、runner/claudemanaged/api_test.go |

表のパスは `internal/` 以下。slug や原文保持の期待値は fake が作る出力で確かめる。実モデルがプロンプトどおりに編集する保証を、このテスト結果から主張しない。

## 今回行わない作業

push、PR 作成、リリース、実データの削除、運用者の network 設定、実 Google／GitHub／managed-agent API と実モデルの試験は行わない。新しい metrics は追加しない。実環境の導入は [運用文書](WORKERS.md) に従う。
