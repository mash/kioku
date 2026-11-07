# Kioku

Kioku turns a team's source documents into a shared project wiki. Members put sources into a project: files, Google Docs, GitHub files and issues, and public web pages. The server keeps every source version and generates wiki pages and a link map from them. Members pull the wiki to a local directory, or read it in the browser and through the Claude connector.

The repository has three parts:

| Part | Path | Role |
| --- | --- | --- |
| Server `kiokud` | `cmd/kiokud`, `internal/` | Stores sources, runs wiki generation, serves the web viewer, the API and the MCP connector |
| CLI `kioku` | `cmd/kioku`, `internal/cli` | Logs in, puts sources, pulls the wiki of one project into the current directory |
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
2. Copy `.env.example` to `.env`. Set `KIOKU_BASE_URL`, the OAuth client ID and secret, and `KIOKU_ENCRYPTION_KEY` from `openssl rand -base64 32`. Keep the key; the server cannot read its data without it.
3. Build and start the server with a persistent volume:

   ```sh
   docker build --target final -t kioku .
   docker run -d --name kioku -p 8080:8080 --env-file .env -v kioku-data:/data kioku
   curl -f http://127.0.0.1:8080/healthz
   ```

4. Put a TLS proxy in front of port 8080 for the origin in `KIOKU_BASE_URL`.
5. Log in with `kioku --server <KIOKU_BASE_URL> login`.

The server keeps its catalog in SQLite and needs one host with a persistent disk. Set `KIOKU_STORAGE=local` to store project data under `/data/objects`, or `KIOKU_STORAGE=s3` with the required `S3_*` settings to use S3. These backends are mutually exclusive; S3 does not use a local content cache. Wiki generation with Anthropic Managed Agents is optional. The sections below describe the settings, storage and backups in detail (in Japanese).

## License

MIT. See [LICENSE](LICENSE).

# 詳細

チームが原資料を投入し、クラウドでWikiとプロジェクトマップを生成・共有するGo製CLI／サーバです。アプリ本体・CLI・マップ表示はGoで実装しています。

```text
メンバーの inbox / Google Docs / GitHub / 公開Artifacts・Sites
                      ↓ put (sources only)
   原資料・追記ログ → 永続ジョブ → Wiki・リンクマップ
                      ↓ 変更ファイルを順次更新
   プロジェクト専用バケット（本文・ログ・ジョブ） + 管理用SQLite
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
# 暗号化キーは初回だけ生成し、以後も同じ値を安全に保存して使う
export KIOKU_ENCRYPTION_KEY="$(openssl rand -base64 32)"
./bin/kiokud
```

API／HTTPで原資料を取得します。クラウドから本文を取得できない場合は、その内容をMarkdownなどのファイルに書いて投入します。ローカルでもGoogle／GitHub認証を使い、認証を省略する起動モードはありません。

別のターミナルで:

```bash
./bin/kioku --server http://127.0.0.1:8080 login --provider google
./bin/kioku --server http://127.0.0.1:8080 create my-project
./bin/kioku share list
./bin/kioku share invite bob@example.com
```

表示されたプロジェクトディレクトリの `inbox/` にMarkdownなどを置きます。

```bash
./bin/kioku put --wait
./bin/kioku generate --wait
./bin/kioku status
./bin/kioku pull
./bin/kioku open
```

プロジェクトのメンバーは、投入された全原資料と生成Wikiを閲覧できます。投入時の追加フラグや個別の共有設定はありません。投入した内容は設定したLLM事業者にも送信されます。変更のないinboxファイルは再送しません。

```bash
kioku put ./notes.md ./export/
kioku put --wait ./notes.md   # Wait for source storage only
kioku generate --wait         # Generate now and pull the wiki
kioku job status <job-id>
kioku job wait <job-id>
kioku job retry <job-id>
kioku join <invite-url>
kioku list
kioku pull <organization/project>
kioku share remove bob@example.com
```

フラグは位置引数の前、グローバルフラグはコマンドの前に指定します。招待は7日間有効・一度限りです。メール指定の招待を受ける際は、そのログインセッションのGoogle／GitHubに現在の確認済みメールを問い合わせて照合します。古いセッションに本人確認情報がない場合や認可が期限切れの場合は、再ログインが必要です。メール送信はせず、招待URLを出力します。

## Google / GitHubの設定

`.env.example` を参考に環境変数を設定します。サーバは `.env` を自動では読みません。認証情報はGitにコミットしないでください。

```bash
export KIOKU_BASE_URL=https://kioku.example.com
export KIOKU_ENCRYPTION_KEY="$(openssl rand -base64 32)"
export GOOGLE_CLIENT_ID=...
export GOOGLE_CLIENT_SECRET=...
export GITHUB_CLIENT_ID=...
export GITHUB_CLIENT_SECRET=...
./bin/kiokud --addr 0.0.0.0:8080 --data /var/lib/kioku
```

OAuthアプリのcallback URL:

- Google: `https://kioku.example.com/auth/google/callback`
- GitHub: `https://kioku.example.com/auth/github/callback`

開発時は `http://127.0.0.1:8080/auth/<provider>/callback` を登録します。GoogleではDocs / Drive APIを有効化し、OAuthテストユーザーを登録してください。外部公開時はGoogleによるスコープ審査が必要になる場合があります。

```bash
kioku --server https://kioku.magicians.ee login --provider google
kioku login --provider github
kioku whoami
kioku connect google
kioku connect github
```

`kioku login` は CLI と同じ端末のブラウザで認証する必要があります（SSH 先や別端末で開く使い方には対応しません）。

ログインと原資料への認可は別です。GoogleはDocs／Driveの読み取りスコープ、GitHubはOAuth Appの `read:user user:email repo` を使用します。`repo` は非公開資料へのアクセスを含む広い権限です。選択リポジトリだけを許可するGitHub App方式はこの実装の対象外です。

`connect` は別のログイン方法を同じユーザーに紐づける操作も兼ねます。既存アカウントでログインしてから実行してください。メール一致だけでアカウントを自動統合しません。CLI認証はブラウザでのOAuth＋一度だけ受領できるトークン交換、stateとPKCEの検証を使用します。

## 原資料

```bash
kioku put 'https://docs.google.com/document/d/ID/edit'
kioku put 'https://github.com/owner/repo/blob/main/README.md'
kioku put 'https://公開されたサイトのURL'
kioku open
```

原資料の閲覧・本文の確認・ダウンロードは、`kioku open` で開く認証済みWeb Wikiビューアーを使います。URLの内容を再取得する場合は、同じURLを `kioku put` でもう一度投入してください。

| 種類 | 取得するもの |
| --- | --- |
| ファイル | UTF-8のMarkdown、テキスト、HTML、JSON、Go/JS/TS等のコード、YAML、CSV、CSS |
| Google Docs | 本文・表・タブのテキストと、取得時点のAPIレスポンス |
| GitHub | ファイル、リポジトリURLのREADME、Issue／PRの説明と先頭100件までのIssueコメント |
| 公開Claude Artifacts／Codex Sites等 | HTTPで取得できるHTML／テキスト。取得できない内容はMarkdown等に書いてファイル投入 |

公開URL取得は認証済みブラウザのCookieを使用しません。ログイン必須ページはエクスポートファイルを投入してください。画面のボタンを操作してしか出ない内容・アプリ内部のコードやデータを完全に収集する機能ではありません。GitHubのPR差分・インラインレビュー・リポジトリ全体の自動収集も範囲外です。`/` を含むブランチ名はcommit SHAのファイルURLを使ってください。

公開URL取得はHTTPSに限定し、DNS解決したIPに固定して接続します。プライベート／予約アドレス・任意ポートを拒否します。JavaScriptの実行やブラウザ自動操作は行いません。

クラウドから本文を取得できない場合は、原資料の内容をファイルに書き、元URL・取得日を添えて投入します。本文を取得できていない状態で、内容を推測したりURLだけを原資料として扱ったりしません。

```markdown
# Artifactの内容

Source: https://元資料のURL
Retrieved: 2026-10-30

（ユーザーが提供した原資料の本文）
```

```bash
kioku put ./artifact-notes.md
```


原資料は取得時点の版を保存します。同じURLを更新したときは最後に取得した内容をWiki生成に使用し、過去の原本は保存します。A → B → Aと戻った場合もAを現在の版とし、原本自体は重複保存しません。

## Wikiとマップ

Managed Agents 未設定では原資料ページ・索引・リンクマップを生成します。意味的な統合と矛盾の整理には Anthropic Managed Agents を設定します。3 変数は一緒に設定し、一部だけなら起動時にエラーになります。

```bash
export ANTHROPIC_API_KEY=...
export KIOKU_MA_AGENT_ID=agent_...
export KIOKU_MA_ENVIRONMENT_ID=env_...
# 任意: 0 は最新の agent バージョン。未設定の webhook は 30 秒ごとの照合で補います。
export KIOKU_MA_AGENT_VERSION=0
export ANTHROPIC_WEBHOOK_SIGNING_KEY=...
```

kiokud は原資料・差分・既存 Wiki をジョブ専用の tar にまとめ、隔離されたエージェントが取得して生成物を返します。通常は増分生成です。`kioku generate --full --wait` は同じ原資料でも全体を作り直します。出力は完全なページ集合で、省略されたページは削除されます。原資料ページと出典一覧・リンクマップはサーバーが付けます。重要な判断は引用元の原資料を確認してください。

生成物は `wiki/*.md`、`maps/graph.json`、`maps/graph.mmd`。Web画面でMarkdownとクリックできるSVGマップを表示し、ページを検索できます。原資料の抽出テキストと原本も、プロジェクト権限を確認して配信します。未信頼HTMLやJavaScriptを閲覧者のブラウザで実行しません。

保存前に容量と待機ジョブ数を予約し、拒否された投入ではオブジェクトを書きません。途中失敗・中断した投入は永続的な予約として残し、削除が完了するまで容量に計上します。再起動後も回収を再試行します。`kioku usage` で予約容量も確認できます。

現在の上限は1原資料4 MiB、1投入30ファイル／JSON全体16 MiB、取得するWiki全体64 MiB。入力 tar は展開時256 MiBまで。エージェント出力はgzip 16 MiB・展開 tar 64 MiB・最大300ページ・各256 KiBまでです。原資料ページを加えたWiki全体も64 MiBまでです。超過は失敗として明示します。

## Codex / Claude Code用スキル

スキルとインストール手順は [plugin/](plugin/README.md) にあります。

## 保存構成: 管理情報はSQLite、プロジェクト情報は選択したストレージ

```text
KIOKU_DATA/
  catalog.sqlite       # アカウント・組織・権限・プロジェクト設定
  admin.sock           # オペレーター専用、0600
  objects/projects/    # KIOKU_STORAGE=local の場合のみ
```

原資料・原資料メタデータ・Wiki・マップ・追記ログ・ジョブ・生成進捗はプロジェクトの保存先に置きます。SQLiteには本文、ファイル一覧、生成チェックポイントを保存しません。プロジェクト専用SQLiteは不要になりました。

```bash
export KIOKU_STORAGE=s3
export S3_ENDPOINT='https://storage.example.com'
export S3_REGION='your-region'
export S3_BUCKET=kioku-projects
export S3_ACCESS_KEY_ID=...
export S3_SECRET_ACCESS_KEY=...
```

`KIOKU_STORAGE=local|s3` または `kiokud --storage=local|s3` で保存先を選択します（指定必須、フラグ優先）。localではS3設定を拒否し、s3では資格情報の不足を拒否します。保存先障害時の自動切り替えはありません。既存環境も新しい設定名へ更新し、`KIOKU_STORAGE` を明示してください。S3互換サービスの違いは `S3_ENDPOINT`・`S3_REGION` などの設定値で指定します。設定変更はデータ移行を行わないため、既存データの切り替えには停止・データ移送・カタログの保存先情報の整合が必要です。旧 `cache/` は使用しません。

事前作成した非公開の `S3_BUCKET` に、`projects/<project-id>/` プレフィックスでデータを保存します。バケット名とルートはSQLiteに記録します。S3 の資格情報は対象バケットの Object Read & Write に限定できます。バケットの作成・削除はアプリでは行いません。

```text
<project-bucket>/projects/<project-id>/
  sources/<source-id>/{original,extracted.txt}
  wiki/index.md
  wiki/*.md
  wiki/inbox/<sequence>-<entry-id>.json  # putの追記専用ログ
  maps/graph.{json,mmd}
  .kioku/state.json                    # 原資料・現在ファイル・進捗
  .kioku/jobs/<job-id>.json
  .kioku/audit/<event-id>.json
```

Wikiは一組だけを保持し、生成中に変更ファイルを順次上書きします。閲覧中に新旧ページが混在することを許容します。`wiki_incomplete` と処理済み原資料バージョンはS3に保存し、失敗時の再実行を可能にします。追記ログは生成で書き換えず、最新64件を `wiki/inbox.md` としてpullできます。

S3使用時は本文のローカルキャッシュを作成しません。SQLite用の永続ディスクは引き続き必要です。S3への保存が成功してからputを受理済みにし、変更のないファイルはアップロードしません。再起動時はS3から状態を読み込みます。カタログのプロセスロックは単一VPSを前提としており、別VPSとの同時書き込みや自動フェイルオーバーには対応しません。

## 移行・オンラインバックアップ

旧プロジェクトSQLiteをS3に移行する前に `DATA/migration-backup/` に保護したアーカイブを作成します。旧 `metadata.json` は `.migrated` として保持します。現在のWikiだけを通常のパスへ移し、ページ履歴は保持しません。移行元が欠損・破損していれば起動を拒否します。旧共有S3バケットから専用バケットへの移動は別途必要です。

稼働中のサーバへオンラインバックアップを依頼できます。

```bash
kiokud --data /var/lib/kioku --online-backup /var/backups/kioku/config-2026-10-31
# 停止時の設定バックアップも可能
kiokud --data /var/lib/kioku --backup /var/backups/kioku/offline-2026-10-31
```

バックアップは設定・アカウントのSQLiteのみです。本文やジョブはS3に残り、過去のSQLiteを復元しても新しいプロジェクトデータを巻き戻しません。暗号化キーとS3接続設定は別途復元してください。保存先はサービスアカウントが書き込める新規ディレクトリにします。バックアップの定期実行・暗号化・S3転送はまだ構成していません。

詳しい保存境界、障害復旧、移行、オンラインバックアップは [WORKERS.md](docs/WORKERS.md) を参照してください。

## 運用・削除・接続確認

```bash
kioku usage
kioku audit                             # ownerのみ
kioku maintenance             # owner: currentを含む20リビジョンを保持、GC
kioku source remove --yes <source-id>   # owner: 原本と旧生成物を削除し再生成
kioku delete --yes                      # owner: プロジェクトと専用バケットを削除
kioku doctor                            # ストレージ往復とGoogle/GitHub接続確認
kioku doctor --llm                      # Managed Agents の設定確認（API 呼出しなし）
```

資料削除では、削除した内容が旧Wikiに残らないようそのプロジェクトの全生成履歴を破棄して再生成します。プロジェクト削除は途中失敗時にも同じコマンドで再実行できます。メンバーが既に取得したローカルコピーや外部バックアップまでは遠隔消去できません。

`maintenance` は未参照オブジェクトを回収し、完了／失敗ジョブを30日、監査履歴を90日で整理します。必要な頻度で明示的に実行・スケジュールしてください。原資料投入／Wiki公開とGCはプロジェクトごとに排他制御し、権限確認と本文の読み込みをロック取得前に行います。HTTP応答の送信中に共通DBのロックは保持しません。オブジェクトストアのバージョニングを使う場合、旧バージョンの保持・削除もプロバイダ側で設定してください。

```bash
kiokud --max-sources 200 --max-source-bytes 67108864 --max-jobs 20 --rate-per-minute 600
```

これらはプロジェクトごとの原資料・待機ジョブ上限と、接続元IPごとの1分間のリクエスト上限です。認証開始は1 IPあたり20回／分。転送ヘッダーを信用せず、実接続元を使用します。プロキシ経由では全員が同じIPになるため、適切な上限と入口側の制限を設定してください。

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
go build ./cmd/...
```

CLI/APIの連携、取得不能なURLのファイル投入案内、SQLiteの分離・移行・障害復旧・バックアップ、署名付きバケット操作、同時投入、招待・権限剥奪、OAuth/PKCE、暗号化、削除・容量制限・GC、Markdown／SVGの安全性をテストします。CIにもブラウザは不要です。

Google／GitHub／Managed Agents はモック、S3はTLSテストサーバで検証します。実サービスの接続は各アプリの登録と資格情報を設定し、`connect` と手順書の実セッションで検証してください。`doctor --llm` は設定の確認だけです。未設定の資格情報による実接続を「検証済み」とは扱いません。

## Organizations

Named projects use `organization/project`:

```bash
kioku org create acme
kioku create acme/platform
kioku pull acme/platform
```

The web path is `/acme/platform`. The caller chooses a working directory; `.kioku/config` in that directory stores the server and namespace. Server-side project IDs and bucket mappings remain internal.

## Cloud operations and releases

- [Releases](docs/RELEASES.md): one version for the server, the CLI and the plugin; a version bump in `plugin.json` makes the release; macOS/Linux amd64/arm64 archives and checksums.
- [Project-scoped cloud agents](docs/CLOUD_AGENTS.md): expiring read/write credentials, origin pinning, revocation, and the separate VM/container sandbox boundary.
- [Claude connector](docs/CLOUD_AGENTS.md#claude-connector): select the Kioku projects to share at consent, then read and search their wikis and save sources through MCP tools in claude.ai chat. For CLI access, exchange a one-time login code, then run `kioku pull`.
- [Account administration](docs/ACCOUNT_ADMIN.md): session revocation, provider management, safe ownership handoff and account deletion.
- [Generation worker architecture](docs/WORKERS.md): durable queue, one generation worker, and independent cleanup maintenance.


`kioku put [URL|FILES...]` saves sources without generating wiki. URL submissions return durable job metadata before retrieval; file submissions return after durable storage. `--wait` on put/source operations waits only for source intake. The server periodically generates changed projects (daily by default, configured with `kiokud --generate-every`). Frequent puts do not postpone the deadline. `kioku generate` requests an earlier refresh, preserving the current revision and making no AI call if sources are unchanged; `kioku generate --wait` waits for publication and pulls the current wiki. Readers can see partial updates while generation runs. `kioku job wait JOB_ID` pulls only when the job is a generation. See [source intake and generation](docs/WORKERS.md) for queues, freshness, limits and recovery.


The default service origin is `https://kioku.magicians.ee`. Source and wiki update timestamps are available through `kioku status`. For Managed Agents with a user's own API key, see the [BYOK runbook](docs/runbooks/byok.md).

## CLI と skills の責務

CLI は cwd の `.kioku/config`（JSON）を読み、1プロジェクトを操作します。親ディレクトリは探索しません。既定の接続先は `https://kioku.magicians.ee`。別接続先は `--server URL`、別の作業ディレクトリは `-C PATH` をコマンドの前に指定します。既存設定と異なる接続先・namespace は拒否します。

```sh
mkdir -p /path/to/project
cd /path/to/project
kioku pull org/project       # 参加確認後に .kioku/config を作成して取得
kioku pull                   # cwd の設定から同じプロジェクトを取得
```

`create NAME` はクラウドにプロジェクトを作成し、`join INVITE_URL` は招待に参加します。どちらも成功時に cwd の設定を保存します。既に設定がある cwd では、クラウド操作を始める前にエラーにします。参加済みのプロジェクトには `pull org/project` を使います。設定作成・取得をまとめる別コマンドはありません。

認証は CLI が `~/.config/kioku/auth/<server-hash>.json` に接続先別で保存します。プロジェクト設定には認証情報を保存しません。`login` は認証だけを行います。会話の選択状態・保存先の一覧・取得タイミング・複数プロジェクトの巡回・フック重複の抑止・文脈と通知はプラグイン（[plugin/](plugin/README.md)）が担当します。

### ローカル配置と取得

- `wiki/` と `maps/`: cwd 直下の読み取り専用ミラー。取得時にローカル編集を上書きし、サーバーの一覧から消えた生成ファイルを削除します。
- `inbox/`: cwd 直下の原資料投入領域。pull による上書き・削除の対象外です。
- `.kioku/`: config、manifest.json、receipts.json、ファイル更新ロック。取得状態はここに集約します。

初回取得では、Kioku のメタデータがない既存の非空 wiki/maps を上書きしません。管理対象の symlink も拒否します。公開・取得ともページ単位の順次反映なので、新旧混在を許容します。部分失敗でも成功したページは残ります。復旧後の pull で再取得できます。

### 取得結果

`kioku pull [org/project]` は通常の結果を stdout に出力します。変更がなければ「最新です」と表示します。失敗は stderr と非ゼロ終了で報告し、部分成功したファイルは残ります。

### プラグインの運用

Claude Code のプラグインは [plugin/](plugin/README.md) にあります。SessionStart フックは、プラグインと同じバージョンの CLI を取得します。対象の選択と pull はスキルが行い、会話ごとに一度 pull の完了を待ちます。CLI は会話セッションを知りません。

旧コマンドは削除し、互換エイリアス・migration コード・旧設定へのフォールバックは提供しません。

Namespaceのorg名・project名はそれぞれ1〜63文字のASCII小文字英数字と単一ハイフンに限定し、先頭・末尾は英数字とします（例: `acme/team-wiki`）。
