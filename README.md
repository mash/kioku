# Kioku

Kioku turns a team's source documents into a shared project wiki. The user's Claude fetches each source and sends Markdown following the [capture rules](plugin/skills/kioku/references/capture.md). The server stores the text as a fetched inbox item and runs an agent to update the wiki. Published source pages keep the evidence; successful publication consumes the inbox items. Members pull the wiki to a local directory, or read it in the browser and through the Claude connector. The client asserts source_url as provenance; the server does not verify that the text came from that URL.

The repository has three parts:

| Part | Path | Role |
| --- | --- | --- |
| Server `kiokud` | `cmd/kiokud`, `internal/` | Stores inbox items, runs wiki generation, serves the web viewer, the API and the MCP connector |
| CLI `kioku` | `cmd/kioku`, `internal/cli` | Logs in, puts sources, selects a project directory and pulls its wiki |
| Claude plugin | [`plugin/`](plugin/README.md) | Skill and hooks for Claude Code, Claude Desktop, Cowork and claude.ai chat |

Prompts are kept in [`prompts/`](prompts/): the wiki agent definition, generation
templates, and Claude plugin hook messages. The plugin links to this shared
directory so its installed copy includes the hook messages.

The default service is `https://kioku.magicians.ee`. The CLI and the plugin use it unless you give another server.

## Install the CLI

Download the archive for your platform from [GitHub Releases](https://github.com/mash/kioku/releases). Each release has `kioku_<version>_<os>_<arch>.tar.gz` for `darwin` and `linux` on `amd64` and `arm64`, and a `SHA256SUMS.txt` file.

```sh
VERSION=vX.Y.Z OS=darwin ARCH=arm64
BASE=https://github.com/mash/kioku/releases/download/$VERSION
curl -fLO "$BASE/kioku_${VERSION}_${OS}_${ARCH}.tar.gz" -fLO "$BASE/SHA256SUMS.txt"
shasum -a 256 -c --ignore-missing SHA256SUMS.txt   # Linux: sha256sum -c --ignore-missing
tar -xzf "kioku_${VERSION}_${OS}_${ARCH}.tar.gz" kioku
install -m 0755 kioku ~/.local/bin/kioku
kioku version
```

To build from source, run `make build`. It writes `bin/kioku` and `bin/kiokud`. [docs/RELEASES.md](docs/RELEASES.md) describes how releases are built.

## Install the Claude plugin

In Claude Code, run:

```text
/plugin marketplace add mash/kioku
/plugin install kioku@kioku
```

In Claude Desktop or Cowork, add the marketplace `mash/kioku` and install `kioku`. The plugin downloads the CLI release of its own version and checks its SHA-256 sum. [plugin/README.md](plugin/README.md) describes the plugin, its data paths and the use without the plugin.

## Self-hosting

You can run your own server, but self-hosting is not supported. There is no compatibility promise between versions, and no help for operations. The minimal start uses the `final` target of the `Dockerfile`:

1. Register a Google or GitHub OAuth app with the callback `<KIOKU_BASE_URL>/auth/google/callback` or `<KIOKU_BASE_URL>/auth/github/callback`.
2. Copy `.env.example` to `.env`. Set `KIOKU_BASE_URL` and at least one Google or GitHub OAuth client ID and secret. `KIOKU_ENCRYPTION_KEY` is optional: when unset, the server generates and persists a key at `<KIOKU_DATA>/encryption.key` (0600) and reuses it on restart. Preserve this file with your deployment; encrypted credentials cannot be recovered without the same key.
3. Build and start the server with a persistent volume:

   ```sh
   docker build --target final -t kioku .
   docker run -d --name kioku -p 8080:8080 --env-file .env -v kioku-data:/data kioku
   curl -f http://127.0.0.1:8080/healthz
   ```

4. Put a TLS proxy in front of port 8080 for the origin in `KIOKU_BASE_URL`.
5. Log in with `kioku --server <KIOKU_BASE_URL> login`.

The server keeps its catalog in SQLite and needs one host with a persistent disk. Storage defaults to local: project data is stored under `<KIOKU_DATA>/objects` (`./data/objects` without configuration). Set `KIOKU_STORAGE=s3` with the required `S3_*` settings to use S3. These backends are mutually exclusive; S3 does not use a local content cache. Configure a generation runner to generate a wiki; without one, generation jobs fail. The sections below describe the settings, storage and backups in detail (in Japanese).

## License

MIT. See [LICENSE](LICENSE).

# 詳細

チームが原資料を投入し、クラウドでWikiとプロジェクトマップを生成・共有するGo製CLI／サーバです。アプリ本体・CLI・マップ表示はGoで実装しています。

```text
メンバーの inbox / Google Docs / GitHub / 公開Artifacts・Sites
                      ↓ put（1要求につき1項目）
   inbox → 1回のエージェント実行（取得・編集）→ Wiki・リンクマップ
                      ↓ 変更ファイルを順次更新
   プロジェクト専用領域（inbox・Wiki・ジョブ） + 管理用SQLite
                      ↓ pull
   <作業ディレクトリ>/wiki/
```

## ローカル起動

Linux / macOS、Go 1.27.3以上。ブラウザの実行環境は不要です。

```bash
go build -o bin/kioku ./cmd/kioku
go build -o bin/kiokud ./cmd/kiokud
# GoogleまたはGitHubのOAuthアプリを登録して設定（callback URLは下記参照）
export GOOGLE_CLIENT_ID=...
export GOOGLE_CLIENT_SECRET=...
# サーバーは暗号鍵が未設定なら ./data/encryption.key に生成・保存する
./bin/kiokud
```

`KIOKU_ENCRYPTION_KEY` を指定した場合は、その値を優先しファイルには書き出しません。未指定なら保存済みの鍵を読み、初回だけ暗号学的乱数で32バイトの鍵を生成します。保存済みの鍵が壊れている場合は再生成せず起動エラーにします。

ユーザーの Claude が API／HTTP で原資料を取得し、必須の frontmatter を付けた Markdown ファイルを投入します。ローカルでもGoogle／GitHub認証を使い、認証を省略する起動モードはありません。

別のターミナルで:

```bash
NAMESPACE=acme/my-project
export KIOKU_SERVER=http://127.0.0.1:8080
./bin/kioku --server http://127.0.0.1:8080 login --provider google
./bin/kioku org create acme
./bin/kioku -n "$NAMESPACE" --server http://127.0.0.1:8080 project create
./bin/kioku -n "$NAMESPACE" project share list
./bin/kioku -n "$NAMESPACE" project share invite bob@example.com
```

原資料は、ユーザーの Claude が本文を取得して投入します。GitHub、Google Docs、公開 Web など、どの資料も本文と原本 URL を渡します。Kioku は資料取得用の資格情報を保持しません。具体例は[原資料](#原資料)を参照してください。

例えば、GitHub にある Kioku の README を取り込み、Wiki を生成して開くには:

```bash
source_commit=$(gh api 'repos/mash/kioku/commits?path=README.md&per_page=1' --jq '.[0].sha')
source_updated_at=$(gh api "repos/mash/kioku/commits/$source_commit" --jq '.commit.committer.date')
{
  printf '%s\n' '---' "source_url: https://github.com/mash/kioku/blob/$source_commit/README.md" "updated_at: $source_updated_at" '---'
  gh api "repos/mash/kioku/contents/README.md?ref=$source_commit" -H "Accept: application/vnd.github.raw+json"
} > /tmp/kioku-readme.md
./bin/kioku -n "$NAMESPACE" put /tmp/kioku-readme.md
./bin/kioku -n "$NAMESPACE" generate --wait
./bin/kioku -n "$NAMESPACE" status
./bin/kioku -n "$NAMESPACE" pull
./bin/kioku -n "$NAMESPACE" open
```

プロジェクトのメンバーは、公開した source ページと Wiki を閲覧できます。投入時の追加フラグや個別の共有設定はありません。サーバーは投入した内容を設定したLLM事業者にも送信します。

```bash
kioku -n "$NAMESPACE" put ./payment-api-design.md  # Artifactから抽出し、capture.md の規則で準備した設計書
kioku -n "$NAMESPACE" generate --wait         # Generate now and pull the wiki
kioku -n "$NAMESPACE" job status <job-id>
kioku -n "$NAMESPACE" job wait <job-id>
kioku -n "$NAMESPACE" job retry <job-id>
kioku project join <invite-url>
kioku project list
kioku -n "$NAMESPACE" pull
kioku -n "$NAMESPACE" project share remove bob@example.com
```

フラグは位置引数の前、グローバルフラグはコマンドの前に指定します。招待は7日間有効・一度限りです。メール指定の招待を受ける際は、そのログインセッションのGoogle／GitHubに現在の確認済みメールを問い合わせて照合します。古いセッションに本人確認情報がない場合や認可が期限切れの場合は、再ログインが必要です。メール送信はせず、招待URLを出力します。

## Google / GitHubの設定

`.env.example` を参考に環境変数を設定します。サーバは `.env` を自動では読みません。認証情報はGitにコミットしないでください。

```bash
export KIOKU_BASE_URL=https://kioku.example.com
# KIOKU_ENCRYPTION_KEY は任意。未設定なら KIOKU_DATA/encryption.key を使用
export GOOGLE_CLIENT_ID=...
export GOOGLE_CLIENT_SECRET=...
export GITHUB_CLIENT_ID=...
export GITHUB_CLIENT_SECRET=...
./bin/kiokud --addr 0.0.0.0:8080 --data /var/lib/kioku
```

OAuthアプリのcallback URL:

- Google: `https://kioku.example.com/auth/google/callback`
- GitHub: `https://kioku.example.com/auth/github/callback`

開発時は `http://127.0.0.1:8080/auth/<provider>/callback` を登録します。Google の OAuth アプリをテストする場合はテストユーザーを登録してください。ログインには Docs／Drive API は不要です。

```bash
kioku login --provider google
kioku login --provider github
kioku whoami
```

`kioku login` は CLI と同じ端末のブラウザで認証する必要があります（SSH 先や別端末で開く使い方には対応しません）。

`login`・`agent login` では、標準サーバー（`https://kioku.magicians.ee`）以外への初回認証時に接続先を表示し、信頼するか確認します。承認はユーザーの `~/.config/kioku/auth/` に接続先ごとに保存し、次回から確認を省略します。別の接続先には改めて承認が必要です。未承認の接続先は、`--server ORIGIN` や `KIOKU_SERVER` で指定した場合も端末での承認が必要です。非対話実行で未承認の接続先を選ぶと、通信前に終了します。明示した接続先と `.kioku/config` が異なる場合は拒否します。通常の `pull` などは引き続き `.kioku/config` の接続先を使用します。

Google と GitHub は Kioku へのログインに使います。Google のスコープは `openid email profile`、GitHub は `read:user user:email` です。ログインは同じ端末のブラウザと loopback、state、PKCE を使います。サーバーはログイン用の資格情報を保持し、アカウント操作時の本人確認に使います。

## 原資料

### プロジェクトの資料アクセス方針

ユーザーの Claude が自身のコネクターや CLI で資料を取得し、本文を送ります。Kioku は受け取った本文をプロジェクトのメンバーへ共有します。`source_url` はクライアントが申告する原本 URL です。サーバーは本文がその URL から来たことや、投稿者の原本への閲覧権限を検証しません。[資料アクセス方針](docs/ACCOUNT_ADMIN.md#project-source-authorization-policy)を参照してください。

### 1. 本文と原本 URL・最終更新日時を渡す

どの資料も、ユーザーの Claude が本文を読み、UTF-8 の Markdown ファイルとして投入します。CLI 単体で使う場合も、利用者が本文をファイルに保存して送ります。別の要約や操作記録は投入しません。

ファイル先頭の YAML frontmatter に `source_url` と `updated_at` を付け、[原資料の取得・検証規則](plugin/skills/kioku/references/capture.md)に従って投入します。

```bash
kioku -n "$NAMESPACE" put ./payment-api-design.md
```

ファイルの put は `fetched` の項目を作ります。HTTP は 202 と `{}`、MCP の `source_put` は `{}` を返します。CLI は成功したファイルごとに `accepted <path>` を表示し、終了コードで成功を伝えます。サーバーは本文をそのまま保存し、source_url を取得し直しません。利用者の frontmatter に `status: removed` があっても状態は変わりません。原本 URL がない、または本文を読めない場合は投入を保留します。定期生成を待つか、`generate --wait` で生成・公開・pull を待ちます。

`kioku -n "$NAMESPACE" put FILE...` にはファイルかディレクトリのパスを1つ以上指定します。複数ファイルは1件ずつ別の要求で送ります。put は送信元のファイルを削除しません。途中で失敗した場合は `accepted <path>` を確認し、未受付のファイルだけを指定して再送します。

### 2. 原資料を取り除く

```bash
kioku -n "$NAMESPACE" delete https://example.com/the-original-document
kioku -n "$NAMESPACE" generate --wait
```

delete は `removed` の項目を保存し、CLI は成功した依頼ごとに `removed <SOURCE_URL>` を表示します。次の生成で対応する source ページと参照を除きます。公開済み Wiki を受付時に消す操作ではありません。

待機中の同じ source_url の項目は、新しい put で置き換えます。removed も待機中の fetched を取り消します。実行に固定済みの項目は置き換えず、新しい項目を次の実行に残します。URL は正規化せず完全一致で扱います。

## Wikiとマップ

Wiki 生成には Runner の設定が必要です。Anthropic Managed Agents を使う場合は次を設定します。3 変数は一緒に設定し、一部だけなら起動時にエラーになります。

```bash
export ANTHROPIC_API_KEY=...
export KIOKU_MA_AGENT_ID=agent_...
export KIOKU_MA_ENVIRONMENT_ID=env_...
# 任意: 0 は最新の agent バージョン。未設定の webhook は 30 秒ごとの照合で補います。
export KIOKU_MA_AGENT_VERSION=0
export ANTHROPIC_WEBHOOK_SIGNING_KEY=...
```

kiokud は待機中の inbox を実行に固定し、`JOB.md`、`inbox/<id>.md`、既存の `wiki/` 全体を入力 tar に入れます。`JOB.md` には job ID とプロジェクトの namespace が入ります。自前の exec runner には、入力と出力の URL も渡します。managed runner は添付された入力ファイルを読み、ローカルに出力ファイルを書きます。指示と出力規則はシステムプロンプトにあります。Runner が fetched と removed を処理して Wiki を編集し、全ページを返します。空の inbox はモデルを呼ばず変化なしで終わります。ただし Runner 未設定の生成は失敗します。`generate --full` はありません。

Runner は原資料を `wiki/source-<slug>.md` に残し、frontmatter の `source_url` で既存ページを見つけて同じファイルを更新します。概念・エンティティ・分析・概要ページから source ページを引用します。`wiki/index.md` は種類ごとの目録で、source ページを載せません。`wiki/log.md` は変更を新しい順に100項目まで記録します。ログにトークンや署名付き URL の秘密を書かないことはプロンプトの規則で、サーバーの検査ではありません。

出力は完全な `wiki/*.md` 集合です。サーバーは受け取り時に形式と source_url の対応を検査し、問題があれば400と一覧を返します。公開時は受理済み tar のハッシュを確認し、Markdown をそのまま公開して `maps/graph.json` と `maps/graph.mmd` を作ります。原資料ページをサーバーが後から付け足す処理はありません。

公開に成功した実行の項目だけを inbox から消します。失敗した実行の項目は待機に戻します。公開の途中で停止した場合は `wiki_incomplete` と項目を残します。新しい put が同じ URL の待機項目を作っていた場合は、失敗した古い項目を戻さず新しい項目を優先します。

| 制限 | 動作 |
| --- | --- |
| put 1要求の JSON 全体1 MiB | 超過は413。1要求に1項目だけ受け付ける |
| プロジェクトの inbox 200項目 | 固定中も数える。追加で超える場合は429 |
| `--max-wiki-bytes`（既定64 MiB） | 公開後の合計で判定。超過中の fetched は413、removed は受付可能 |
| 出力 tar の圧縮後・展開後それぞれ設定値の2倍 | 受け取り時に400。ページ数・1ページの固定上限はない |
| 入力 tar の展開後256 MiB | 超過した実行は失敗し、項目を残す |

公開は Wiki の上限を超えても成功します。超過中の生成は removed だけを固定し、fetched を待機に残します。removed がなければ実行しません。`kioku -n "$NAMESPACE" project usage` で公開容量と inbox 件数を確認します。公開容量の `Revision.StoredBytes` は派生した maps も含みます。入力の全資料が256 MiBに収まらない場合は、不要な待機資料を取り消すなどして入力を減らす必要があります。

## Codex / Claude Code用スキル

スキルとインストール手順は [plugin/](plugin/README.md) にあります。

## 保存構成: 管理情報はSQLite、プロジェクト情報は選択したストレージ

```text
KIOKU_DATA/
  catalog.sqlite       # アカウント・組織・権限・プロジェクト設定
  encryption.key       # 暗号鍵の環境変数が未設定なら初回生成、0600
  admin.sock           # オペレーター専用、0600
  objects/projects/    # KIOKU_STORAGE=local の場合のみ
```

inbox・Wiki・マップ・ジョブ・生成進捗はプロジェクトの保存先に置きます。SQLiteには本文、ファイル一覧、生成チェックポイントを保存しません。プロジェクト専用SQLiteは不要になりました。

```bash
export KIOKU_STORAGE=s3
export S3_ENDPOINT='https://storage.example.com'
export S3_REGION='your-region'
export S3_BUCKET=kioku-projects
export S3_ACCESS_KEY_ID=...
export S3_SECRET_ACCESS_KEY=...
```

`KIOKU_STORAGE=local|s3` または `kiokud --storage=local|s3` で保存先を選択します（未指定はlocal、フラグ優先）。保存ディレクトリも未指定なら `./data/objects/` を使います。localではS3設定を拒否し、s3では資格情報の不足を拒否します。保存先障害時の自動切り替えはありません。S3を使う場合は `KIOKU_STORAGE=s3` を明示してください。S3互換サービスの違いは `S3_ENDPOINT`・`S3_REGION` などの設定値で指定します。設定変更はデータ移行を行わないため、既存データの切り替えには停止・データ移送・カタログの保存先情報の整合が必要です。旧 `cache/` は使用しません。

事前作成した非公開の `S3_BUCKET` に、`projects/<project-id>/` プレフィックスでデータを保存します。バケット名とルートはSQLiteに記録します。S3 の資格情報は対象バケットの Object Read & Write に限定できます。バケットの作成・削除はアプリでは行いません。

```text
<project-bucket>/projects/<project-id>/
  .kioku/inbox/<id>.md                 # fetched の本文。公開後に回収
  wiki/index.md
  wiki/*.md
  wiki/source-<slug>.md
  wiki/log.md
  maps/graph.{json,mmd}
  .kioku/state.json                    # inbox・現在ファイル・進捗
  .kioku/jobs/<job-id>.json
  .kioku/audit/<event-id>.json
```

Wikiは一組だけを保持し、生成中に変更ファイルを順次上書きします。閲覧中に新旧ページが混在することを許容します。`wiki_incomplete` と実行に固定した inbox の ID を保存し、失敗時の再実行を可能にします。pull は公開済みの wiki/maps だけを取得します。生成前の受付状態は `kioku -n "$NAMESPACE" status` の `inbox_count` と `inbox`（`source_url`・`status`）で確認します。既存の `GET /api/projects` がプロジェクトごとに返す値を表示し、専用の inbox GET は設けません。

S3使用時は本文のローカルキャッシュを作成しません。SQLite用の永続ディスクは引き続き必要です。S3への保存が成功してからputを受理済みにし、変更のないファイルはアップロードしません。再起動時はS3から状態を読み込みます。カタログのプロセスロックは単一VPSを前提としており、別VPSとの同時書き込みや自動フェイルオーバーには対応しません。

## 旧データ・オンラインバックアップ

旧 content schema 1 を検出すると起動を拒否します。旧プロジェクト内容は、運用者がサーバーを停止して手作業で削除する必要があります。更新後は利用者へ put し直しを依頼してください。

稼働中のサーバへオンラインバックアップを依頼できます。

```bash
kiokud --data /var/lib/kioku --online-backup /var/backups/kioku/config-2026-10-31
# 停止時の設定バックアップも可能
kiokud --data /var/lib/kioku --backup /var/backups/kioku/offline-2026-10-31
```

バックアップは設定・アカウントのSQLiteのみです。本文やジョブはS3に残り、過去のSQLiteを復元しても新しいプロジェクトデータを巻き戻しません。暗号化キーとS3接続設定は別途復元してください。自動生成した `encryption.key` もSQLiteバックアップには含まれないため、別途保管・復元が必要です。保存先はサービスアカウントが書き込める新規ディレクトリにします。バックアップの定期実行・暗号化・S3転送はまだ構成していません。

詳しい保存境界、障害復旧、オンラインバックアップは [WORKERS.md](docs/WORKERS.md) を参照してください。

## 運用・削除・接続確認

```bash
kioku -n "$NAMESPACE" project usage
kioku -n "$NAMESPACE" project audit                  # owner のみ
kioku -n "$NAMESPACE" project maintenance            # owner: 未参照オブジェクトを回収
kioku -n "$NAMESPACE" delete https://example.com/doc  # 資料の削除を inbox に投入
kioku -n "$NAMESPACE" project delete --yes           # owner: プロジェクトを削除
```

資料の削除は次の生成で反映します。プロジェクト削除は実行・資格情報の片付けを待ち、途中失敗時にも同じコマンドで再実行できます。取得済みのローカルコピーや外部バックアップは遠隔消去できません。

`project maintenance` は未参照の fetched 本文を含むオブジェクトを回収し、完了／失敗ジョブを30日、監査履歴を90日で整理します。必要な頻度で実行・スケジュールしてください。オブジェクトストアの旧バージョンは、プロバイダー側でも保持・削除を設定します。

```bash
kiokud --max-wiki-bytes 67108864 --max-jobs 20 --rate-per-minute 600
```

Wiki の容量と待機ジョブ数はプロジェクトごとに制限します。リクエスト数は接続元 IP ごとに毎分制限し、認証開始は20回／分です。転送ヘッダーを信用せず実接続元を使うため、プロキシ経由では入口側の制限も設定します。

`GET /healthz` はメタデータの正常性を確認します。構造化アクセスログに認証トークン・招待トークン・クエリは出力しません。Prometheus形式の `GET /metrics` は `KIOKU_METRICS_TOKEN` をBearerで要求し、未設定なら利用できません。

## Docker イメージ

既定のDockerイメージは非rootで動作し、ブラウザを含みません。永続ボリュームとTLS終端が必要です。ブラウザを導入する追加イメージや設定はありません。

```bash
docker build -t kioku .
docker run --rm -p 8080:8080 --env-file .env -v kioku-data:/data kioku
```

S3互換ストレージを保存先として使えます。Cloudflare ContainersやCloud RunのエフェメラルディスクをSQLiteの保存先にするとデータを失うため、この構成をそのまま配置してはいけません。デプロイ先はSQLiteを永続化できる単一サーバを前提とします。

## 検証

```bash
go test -race ./...
go vet ./...
go build ./...
```

CLI/API/MCP の inbox 受付、単一実行、出力の source_url 検査、公開・再起動・再試行、容量制限・GC、ジョブトークンの秘密の非露出、認証・権限・バックアップ、Markdown／SVG の安全性をテストします。CIにもブラウザは不要です。

Google／GitHub のログインと Managed Agents はモック、S3 は TLS テストサーバーで検証します。実サービスのログインと生成は、各アプリの登録と資格情報を設定して手順書の実セッションで検証してください。ローカルテストは実サービスの検証を含みません。

## Organizations

Named projects use `organization/project`:

```bash
NAMESPACE=acme/platform
kioku org create acme
kioku -n "$NAMESPACE" project create --description "Platform development"
kioku -n "$NAMESPACE" pull
```

The web path is `/acme/platform`. `-n ORG/PROJECT` selects the projects.json path for the same server and namespace, or `~/.local/share/kioku/projects/ORG/PROJECT`, and creates it with mode 0700 if needed. The CLI records it in `~/.config/kioku/projects.json`; `.kioku/config` in that directory stores the server and namespace. Combine `-C DIR` with `-n ORG/PROJECT` to use an existing directory. Create requires `-n`. Join uses cwd or `-C DIR` and rejects `-n`. Command flags must precede positional arguments. Server-side project IDs and bucket mappings remain internal. Public project API paths use `/api/projects/<organization>/<project>`, and responses identify projects by `namespace`.

## Cloud operations and releases

- [Releases](docs/RELEASES.md): one version for the server, the CLI and the plugin; a version bump in `plugin.json` makes the release; macOS/Linux amd64/arm64 archives and checksums.
- [Project-scoped cloud agents](docs/CLOUD_AGENTS.md): expiring read/write credentials, origin pinning, revocation, and the separate VM/container sandbox boundary.
- [Claude connector](docs/CLOUD_AGENTS.md#claude-connector): select the Kioku projects to share at consent, then read and search their wikis and save sources through MCP tools in claude.ai chat. For CLI access, exchange a one-time login code, then run `kioku -n "$NAMESPACE" pull`.
- [Account administration](docs/ACCOUNT_ADMIN.md): session revocation, provider management, safe ownership handoff and account deletion.
- [Generation worker architecture](docs/WORKERS.md): durable queue, one generation worker, and independent cleanup maintenance.


`kioku -n "$NAMESPACE" put FILE...` saves inbox items without fetching or generating. The scheduler runs projects with waiting items daily by default (`--generate-every`); frequent puts do not postpone the deadline. `kioku -n "$NAMESPACE" generate --wait` waits for publication and pulls the wiki. Inspect `kioku -n "$NAMESPACE" status` for inbox items, `wiki_updated_at` and `wiki_incomplete`. There is one mutable wiki; readers can see partial updates during publication.

CLI API requests send `Kioku-Client-Version`. The server rejects missing or older versions with 426; this release requires 0.1.0. Browser sessions and MCP are exempt. Upgrade the server, CLI and plugin together. Development builds marked `dev` bypass the version gate. No compatibility aliases are provided.

## CLI と skills の責務

CLI（コマンドラインの操作ツール）は `-n ORG/PROJECT` で保存先を選び、1プロジェクトを操作します。接続先は `--server URL`、`KIOKU_SERVER`、既定の `https://kioku.magicians.ee` の順で決めます。CLI は `~/.config/kioku/projects.json` の同じ server・namespace（organization/project 形式のプロジェクト名）の path を使い、項目がなければ `~/.local/share/kioku/projects/<organization>/<project>` を使います。保存先がなければ CLI が0700で作ります。

```sh
kioku -n "$NAMESPACE" pull        # CLI が保存先を作り、参加確認後に紐づけて取得
kioku -n "$NAMESPACE" status   # どの作業ディレクトリからでも同じ対象を参照
```

紐づけとは、CLI が保存先の `.kioku/config` に接続先と namespace を書き、そのディレクトリを1プロジェクトに結ぶことを指します。親ディレクトリは探索しません。既存の紐づけが接続先や namespace と異なる場合、CLI は設定を書き換えずに止まります。未紐づけでは pull、認証用コマンド、project create を使えます。それ以外では CLI が先に `kioku -n "$NAMESPACE" pull` を実行するよう案内します。

`kioku -n "$NAMESPACE" project create --description "プロジェクトの説明"` はクラウドにプロジェクトを作り、保存先へ紐づけます。create は `-n` を必須とし、pull と create は namespace の位置引数を受け付けません。`-n` を省略すると、CLI は cwd（作業ディレクトリ）か `-C DIR` の `.kioku/config` を使います。`project join INVITE_URL` は cwd か `-C DIR` へ紐づけ、`-n` を受け付けません。create と join は既存設定があればクラウド操作の前に止まります。参加済みのプロジェクトには pull を使います。

ユーザーが保存先を明示した場合、スキルは `kioku -C DIR -n "$NAMESPACE" pull` を使います。DIR は既存ディレクトリを指定し、未紐づけか同じ接続先・namespace への紐づけを必要とします。以後も全プロジェクト操作で `-n "$NAMESPACE"` を使い、明示した保存先を使う操作では `-C DIR` を併用します。

CLI は create・join・pull・agent login の紐づけ成功時と、毎回の pull で projects.json に項目を足します。project delete の成功時は項目を消します。同じ server・namespace の既存 path は変えず、別の場所へ紐づけた場合は既存 path を stderr（標準エラー出力）に示します。create・join・pull は `--description TEXT` で説明を保存します。join ではフラグを招待 URL の前に置きます。ファイルを0600、作成する設定ディレクトリを0700にします。JSON をデコードできない、path が絶対パスでない場合は、ファイル名を示して止まります。projects.json の書き込みだけに失敗した場合は終了コード0で警告します。登録失敗には次の pull、削除失敗にはファイルの修復と該当項目の削除を案内します。

CLI が認証を `~/.config/kioku/auth/<server-hash>.json` に接続先別で保存します。プロジェクト設定に認証情報は保存しません。login は認証だけを行います。プラグイン（[plugin/](plugin/README.md)）は会話内の対象選択、取得タイミング、複数プロジェクトの巡回、結果の説明を担当します。スキルは projects.json を読むだけにします。新規作成時や description がない時は、スキルがプロジェクト名・会話・Wiki から説明案を作り、ユーザーの確認か修正を受けて `--description` で渡します。

### ローカル配置と取得

スキルは `mktemp -d` を単独の Bash 呼び出しで実行し、出力した絶対パスをそのまま使います。シェル変数や `$(...)` は使いません。一時ディレクトリに原資料の Markdown を書き、`kioku -n "$NAMESPACE" put FILE` に実際のファイルパスを指定して送信します。

- `wiki/` と `maps/`: 保存先直下の読み取り専用ミラー。取得時にローカル編集を上書きし、サーバーの一覧から消えた生成ファイルを削除します。
- `.kioku/`: config、ファイル更新ロック。config は server と namespace のみを保存し、Git で共有できます。

設定の初回作成では、既存の非空 wiki/maps があれば紐付けを拒否します。config がある場合、wiki/maps は管理対象のミラーとして扱うため、独自資料は置かないでください。管理対象の symlink も拒否します。公開・取得ともページ単位の順次反映なので、新旧混在を許容します。部分失敗でも成功したページは残ります。復旧後の pull は実ファイルとサーバーの一覧を比較して再同期します。前回の取得 revision や同期未完了状態はローカルに保存せず、status にも表示しません。

### 取得結果

`kioku -n "$NAMESPACE" pull` は通常の結果を stdout に出力します。変更がなければ「最新です」と表示します。失敗は stderr と非ゼロ終了で報告し、部分成功したファイルは残ります。

### プラグインの運用

Claude Code のプラグインは [plugin/](plugin/README.md) にあります。SessionStart フックは、プラグインと同じバージョンの CLI を取得します。対象の選択と pull はスキルが行い、会話ごとに一度 pull の完了を待ちます。CLI は会話セッションを知りません。

旧コマンドは削除し、互換エイリアス・旧設定へのフォールバックは提供しません。

Namespaceのorg名・project名はそれぞれ1〜63文字のASCII小文字英数字と単一ハイフンに限定し、先頭・末尾は英数字とします（例: `acme/team-wiki`）。

### Claude Managed Agents runner

`KIOKU_RUNNER=claudemanaged` explicitly selects Anthropic Managed Agents for
operator-key generation and requires `ANTHROPIC_API_KEY`, `KIOKU_MA_AGENT_ID`,
and `KIOKU_MA_ENVIRONMENT_ID`. See [runner configuration](docs/WORKERS.md#generation-runner-type).

自分の生成用 API キーを使う場合は kiokud をセルフホストします。利用者ごとの
キー登録画面・API はありません。サーバー設定例:

```bash
export KIOKU_RUNNER=claudemanaged
export ANTHROPIC_API_KEY=...
export KIOKU_MA_AGENT_ID=agent_...
export KIOKU_MA_ENVIRONMENT_ID=env_...
export KIOKU_BASE_URL=https://kioku.example.com
kiokud
```

Agent と Environment は同じ API キーでアクセスできるワークスペースに事前作成します。
そのワークスペース専用の API キーを使います（サーバー設定には workspace ID の指定はありません）。
Agent の system には次のプレースホルダを設定します。

```text
kiokud sends this agent's system prompt with every session. A session without that override must do nothing and end.
```

| 手順 | 操作 |
| --- | --- |
| S1 | console または `ant` で Agent を作成する（`POST /v1/agents`）。model は任意、system は上記の文、tools は不要。 |
| S2 | Environment を作成する（`POST /v1/environments`）。cloud 環境で [`limited` で許可ホストを空にする](docs/WORKERS.md#job-credential-and-network-policy)。 |
| S3 | 差し替える場合だけ、動かす kiokud と同じ版の repo の `prompts/` から、下記3ファイルのうち必要なものを専用の DIR にコピーする。 |
| S4 | DIR 内のファイルを編集する。既定値を使う場合は S3・S4 は不要。 |
| S5 | `KIOKU_MA_AGENT_ID` と `KIOKU_MA_ENVIRONMENT_ID` に作成した ID を設定する。差し替える場合は `KIOKU_PROMPTS_DIR=DIR` も設定する。 |
| S6 | kiokud を再起動する。検証に失敗すると起動しない。起動ログの `source` と `sha256` を確認する。 |

Environment の package managers と MCP servers は無効にし、デプロイごとに別の Environment を作成します。kiokud は各実行前に設定をAPIから読み戻し、外向き通信を許可する環境では生成を開始しません。旧版から切り替える前に稼働中のジョブと後片付けを完了させ、新しい環境のIDを設定してください。

kiokud はセッションごとに `agent_with_overrides` で system、model（指定時）、固定 tools を送信し、`mcp_servers` と `skills` は空配列で置き換えます。Agent 側に追加したツール設定は使いません。`KIOKU_MA_AGENT_VERSION` に値があれば version も送信します。Agent の console は system のプレースホルダを表示するので、実際の文は kiokud の起動ログで特定します。

通常の OAuth・ストレージ設定は [Self-hosting](#self-hosting) を参照してください。

CLI は `kioku --server https://kioku.example.com login` で接続し、
`kioku -n "$NAMESPACE" --server https://kioku.example.com pull` で作業先を設定します。
生成予算はサーバーのセッション上限以内で指定できます。指定しない場合はサーバー上限を使います。

BYOK 対応版から更新する場合は、先に旧版で新規投入を止め、接続解除で既存ジョブの
停止・session/vault cleanup・保存キー削除を完了させてください。未完了のまま更新しません。
旧 BYOK ジョブは更新後に実行・再試行できません。生成が必要なら新規ジョブを作成します。
既存バックアップからのキー消去やプロバイダー側のキー失効は自動では行いません。
従来 BYOK 接続によって停止していた定期生成も通常の対象になるため、再開前に対象と予算を確認します。

### 生成プロンプトと MCP の説明の差し替え

`KIOKU_PROMPTS_DIR` が未設定なら、バイナリに埋め込んだ既定値を使います。設定した場合も、置かなかったファイルは既定値のままです。ファイルは起動時だけ読みます。

| ファイル | 差し替える内容 |
| --- | --- |
| [wiki-agent.system.md](prompts/wiki-agent.system.md) | system 本文全体と、任意の YAML frontmatter の `model`。他の frontmatter キーは不可。 |
| [wiki-agent.user.md](prompts/wiki-agent.user.md) | 最初の user message 全体。frontmatter は不可。`{{.InputURL}}` と `{{.OutputURL}}` の参照が必須。 |
| [mcp-server.yaml](prompts/mcp-server.yaml) | `instructions`、`tools.<名前>.description`、`tools.<名前>.properties.<プロパティ>.description`。書いたキーだけを上書きする。既存 schema 内で説明のないプロパティにも追加できる。 |

`prompts/` 全体はコピーしないでください。DIR 直下の未知の名前（symlink を含む）は起動エラーです。`.` で始まる名前だけを無視します。MCP のツール名・順序・schema と wiki agent の tools は Go に固定されています。`output-rules.txt` と hook の文は差し替え対象ではありません。

全ファイルで `{{.BaseURL}}`、`{{.ClientVersion}}`、`{{.MaxOutputBytes}}` を使えます。値は順に `KIOKU_BASE_URL`、サーバーの `MinimumClientVersion`、`--max-wiki-bytes` の2倍です。`{{.JobID}}`、`{{.Namespace}}`、`{{.InputURL}}`、`{{.OutputURL}}` は user ファイルだけで使えます。system は全ジョブで同じ文になります。`JOB.md` の項目は固定です。managed runner では `BaseURL` は空になり、`InputURL` と `OutputURL` はローカルパスになります。system ファイルには `{{.ReadJobInput}}` と `{{.SubmitJobOutput}}` でrunnerに合った入出力手順を差し込めます。

Go の `text/template` はフィールド参照のみを許可します。`if`、`range`、`with`、パイプ、関数、`define`、`template`、未定義変数は起動エラーです。文字列 `{{` 自体は `{{"{{"}}` と書きます。`$KIOKU_JOB_TOKEN` はそのまま残ります。YAML の説明は先に YAML として解析するので、`{{` で始まる文は引用符または block scalar で書きます。

```yaml
instructions: 社内の記述規則に従ってください。
tools:
  wiki_search:
    description: "{{.BaseURL}} の社内 wiki を検索する"
```

model は文字列、または必須の `id` と任意の `effort`、`speed`、`inference_geo` を持つ map です。各値は文字列で、テンプレート展開はしません。

```yaml
---
model:
  id: claude-opus-5-5
  effort: high
  speed: standard
  inference_geo: us
---
```

MA は指定された model を送信し、省略時は Agent 側の model を使います。model の文字列指定や effort を省いた map は、MA 側の effort をモデル既定に戻し、省略した inference_geo を消します。維持したい値は map に明示してください。exec は model の id だけを使い、他のフィールドがあれば起動時に警告を1件出します。exec で model を省略すると起動しません。OpenAI は frontmatter の model を使わず、`KIOKU_OPENAI_MODEL` を使います。

ディレクトリ不備、未知のキー、解析できない YAML/frontmatter/template、空の description も起動エラーです。MA の system は展開後に `utf8.RuneCountInString` で100,000文字以内であることを検査します。他の文や runner に独自の長さ制限は設けません。エラーは種類と行番号だけを示し、ファイル本文を出しません。

起動ログは3ファイルそれぞれに `source=default|override` と `sha256` を出します。ハッシュは有効な内容の JSON 表現から計算します（system は model を含み、user はテンプレート本文とデプロイ変数を含み、MCP は既定値とのマージ後）。本文自体はログに出しません。差し替えたファイルには、将来の既定値の改善が自動では反映されません。

起動済みの MA・OpenAI セッションは作成時の文を使い続け、retry の新しい attempt は新しい文を使います。作成結果が不明な attempt は既存セッションを検索し、別の文で作り直しません。exec は従来どおり再起動で停止します。MCP は `listChanged` を通知しないため、client は再接続まで古い説明を使う場合があります。既定の `instructions` はありません。

MA がセッション作成を4xxで拒否すると `session create rejected status=... type=...` を記録します。429 は既存の一時エラー扱いを維持し、それ以外の4xxは attempt を失敗させます。プロバイダーのエラー本文はログに出しません。

### OpenAI Agents API runner

`KIOKU_RUNNER=openai` selects the OpenAI-managed agent and hosted sandbox for
operator-key generation. Set `OPENAI_API_KEY` and `KIOKU_OPENAI_MODEL` explicitly.
The application key needs access to Agents sessions and the Files API. The runner
creates a small hosted environment with networking disabled, attaches the
input archive, and checks the effective network policy before starting the turn.
It retrieves the expected artifact from that completed turn. The input archive
must fit within the Files attachment limit of 50 MiB.

Kioku sends the per-job budget as `spend_control.limit`. Provider spend is
provisional, so the runner leaves the common settled cost unknown.
After completion or cancellation, Kioku deletes artifacts, the session and
the input file. Keep the configured credentials and runner type available
until pending cleanup finishes.

This integration is covered by local HTTP contract tests; real API permissions,
budget enforcement, hosted networking, and generated wiki quality still require
an isolated live smoke test before production rollout.
See the [Agents API documentation](https://developers.openai.com/api/docs/guides/agents-api/overview).

## Codex plugin

[Codex plugin](codex-plugin/README.md) bundles the skill, session-start hook, and matching CLI. Release archives can be installed from a local marketplace without adding Kioku to PATH.
