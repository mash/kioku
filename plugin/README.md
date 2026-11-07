# Kioku plugin for Claude

会話の対象・ローカル保存先・取得タイミングを管理するプラグイン。
CLI は cwd の `.kioku/config` に従って1プロジェクトを操作する。

## 責務

| スキル | CLI |
| --- | --- |
| 会話内のプロジェクト選択 | 接続先別の認証 |
| カタログと cwd の指定 | `.kioku/config` の読み書き |
| 対象選択直後の pull、人のログイン後の巡回 | 単一プロジェクトの差分取得・再試行 |
| Wiki の参照と結果の説明 | ファイル反映・取得メタデータ・更新ロック |

## CLI の操作

プラグインは `${CLAUDE_PLUGIN_DATA}/bin/kioku` の CLI を使う。
以下の `kioku` はそのパスに置き換える。プラグインなしでは PATH の CLI を使う。

```sh
mkdir -p /path/to/project
cd /path/to/project
kioku pull org/project       # 既定接続先で確認し .kioku/config を作って取得
kioku pull                   # cwd の設定を使って取得
```

接続先の既定は `https://kioku.magicians.ee`。
初回は `kioku --server URL pull org/project` で別の接続先を指定できる。
`project create` はクラウドに新規作成し、`project join` は招待へ参加する。
どちらも cwd に JSON の設定を保存する。CLI は親ディレクトリを探索しない。
既存の異なる設定は上書きしない。

`wiki/`・`maps/`・`inbox/` は cwd 直下、CLI のメタデータは `.kioku/` 内に置く。
CLI はユーザー共通領域で接続先別に認証を管理する。スキルは認証情報を読まない。

## 原資料の投入

GitHub、Google Docs、公開 Web は `kioku put URL`、Artifact など取得できない資料は `kioku put FILE` で原文と source_url を送る。人・agent・MCP で同じ手順を使う。git 文書はファイルを指す URL を使う。操作の要約やイベント記録は別途送らない。

put は inbox の ID と status を返し、取得・生成を待たない。資料の削除は `kioku delete URL`。定期生成か `kioku generate --wait` で Wiki を更新する。取得失敗は Runner が wiki/log.md に書き、利用者が尋ねたときにスキルが読む。詳しくは [capture.md](skills/kioku/references/capture.md) を参照する。

## Claude Code

| 利用環境 | インストール | Kioku MCP の接続 | 初回の動作 |
| --- | --- | --- | --- |
| Claude Code | `/plugin marketplace add mash/kioku` の後に `/plugin install kioku@kioku`。開発時は `claude --plugin-dir /absolute/path/to/kioku/plugin` も使える。 | `/mcp` | SessionStart が取得した CLI で local mode を使う。CLI やサーバーを使えなければ、モード判定に従う。 |
| Claude Desktop / Cowork | Customize > Plugins > Add marketplace に `mash/kioku` を追加し、`kioku` をインストールする。 | プラグインの Connectors タブ | スキルがモードを選ぶ。Cowork のネットワーク接続は未検証。 |
| claude.ai chat | 同じ marketplace からインストールする。 | プラグインの Connectors タブ | remote mode のみ。chat のサンドボックスは Kioku サーバーへ接続できない。 |

claude.ai アカウントのプラグインは Claude Code にも同期する（[公式仕様](https://claude.com/docs/plugins/platform-support.md)）。

プロンプト本文はリポジトリルートの [`prompts/`](../prompts/) に集約する。
`plugin/prompts` はそのディレクトリへのシンボリックリンクで、marketplace からのインストール時にはリンク先の内容がプラグイン内へコピーされる。
プラグイン単体を配布するときは、`cp -RL plugin /path/to/package` などでリンクを実体化する。

SessionStart フックは POSIX sh を使う。Python は不要。
CLI のパスは `${CLAUDE_PLUGIN_DATA}/bin/kioku` の1つだけで、実体の実行ファイルを置く。
startup・resume・clear・fork では、フックが `bin/kioku version` を実行し、次の場合だけプラグインと同じバージョンの CLI を取得する。

- `bin/kioku` がない、実行できない、または `version` が失敗する。
- 入っている CLI のバージョンの文字列が、`.claude-plugin/plugin.json` の `version` に `v` を付けた値と異なる。古くても新しくても取得する。

同じバージョンなら、フックは何もしない。フックはバージョンの新旧を比べない。
compact ではフックが CLI を取得せず、文を出すだけにする。
古いプラグインのセッションも compact でフックを再実行するので、この規則で古いバージョンへ戻る主な経路をなくす。
ただし古いセッションでプラグインの更新後に `/clear` を実行すると、古いバージョンの CLI を一度取得することがある。
次の新しいセッションが、現在のバージョンの CLI を再び取得する。
フックは SHA-256 を検証したファイルを一時ディレクトリに置き、1回の `mv -f` で `bin/kioku` へ差し替える。
既存のファイルには書き込まないので、実行中のプロセスは古いファイルを使い続ける。
プラグインの更新前に始めたセッションも、次のコマンドから新しい CLI を使う。
コマンドの後方互換は提供しない。このリリースは CLI 0.1.0 以上を要求し、サーバーは古い版や版ヘッダーのない API 要求を426で拒否する。更新後は新しいセッションで現在のスキルと CLI を使う。

初回取得には curl、tar、sha256sum または shasum が必要。
対応環境は macOS と Linux の amd64・arm64。
CLI を取得するたびに、一度だけ `github.com` と、その転送先（`release-assets.githubusercontent.com` または `objects.githubusercontent.com`）へのネットワーク接続が必要になる。
取得失敗はセッションを止めない。古い CLI が動けばそれを使い、次の startup・resume・clear・fork が再度取得を試みる。
`plugin.json` の `version` が `X.Y.Z` の形でなければ、フックは取得しない。動く `bin/kioku` がなければ、CLI が無いことを伝える文を出す。
そのバージョンのリリースがまだ無いときも取得に失敗し、フックは今の CLI を使い続ける。

スキルは CLI のパスをそのままコマンド名に書き、変数や `cd` を前に付けない。

開発時は Claude Code の起動前に `KIOKU_CLI=/abs/path/to/kioku` を設定する。
startup・resume・clear・fork では、フックが取得とバージョン確認を省き、そのファイルを `bin/kioku` へコピーする。
コピーも同じ一時ファイルと `mv -f` を使う。compact ではコピーしない。
スキルは `KIOKU_CLI` を読まず、常に `bin/kioku` を使う。
ローカルでビルドした CLI のバージョン（`git describe` の値や `dev`）はプラグインのバージョンと異なる。そのため `KIOKU_CLI` を外すと、次の startup がプラグインと同じバージョンの CLI を取得する。
通常のプラグインは PATH の `kioku` を使わない。
`/reload-plugins` は CLI を取得しない。新しいバージョンの CLI は次の startup などが取得する。

フックは SessionStart だけで動く。
スキルは最初のユーザーメッセージから対象を選び、会話ごとに一度 pull の完了を待つ。
pull の後に `wiki/index.md` と関連ページを grep やファイルツールで読む。
追加の pull は、ユーザーの依頼か、作業に新しい内容が必要な場合だけ実行する。
pull に失敗した場合は、既存コピーを最新と呼ばない。
Claude Code の sandbox を有効にしている場合は、sandbox の設定で `kioku.magicians.ee` への通信と `~/.local/share/kioku/projects`・`~/.config/kioku`・既定以外のプロジェクトの path への書き込みを許す。許さないと pull が失敗する。

選択は会話内で保持し、カタログは `~/.config/kioku/projects.json` に置く。
成功した pull ごとに、スキルが同じ server・namespace の項目の有無を確認する。
項目がなければ path とともに追加し、既存の項目とフィールドを保持する。
[examples/projects.json](examples/projects.json) に形式の例がある。
保存先はカタログの path、ユーザー指定の順で決める。
指定がなければ `~/.local/share/kioku/projects/<organization>/<project>` を使う。
プロジェクトはプラグインのデータ領域の外に置き、アンインストール後も inbox を残す。

人の `kioku login` が成功したら、スキルが `project list` を確認し、各 path を直列に pull する。
1件失敗しても残りを続行し、結果をまとめて伝える。CLI の login 自体は取得しない。
選択中のプロジェクトの pull は会話の初回取得を兼ねる。重ねて pull しない。

## Connector login（ローカル・クラウド共通）

対象サーバーの `/mcp` を Claude の connector に接続する。
既定は `https://kioku.magicians.ee/mcp`。
local mode の namespace はユーザー指定、repository、カタログから確定する。
認証前の `project list` は不要。

スキルは `whoami` の `agent_id` を確認する。人の認証が有効なら使い続ける。
人の認証がなく、有効な agent 認証があれば、agent 認証を使う。
有効な認証がないクラウドセッションでは connector login を使う。
ローカル端末では人に `!` 付きのログインを頼み、完了を待つ。Claude は login を実行しない。
`!` が使えない環境（Claude Desktop や Cowork など）では connector login を使う。
人の認証がある状態で人が connector login を頼んだ場合は、`kioku logout` が人のセッションを終えることを説明し、同意を得てから logout する。

connector login は `cli_login_code` → `kioku agent login CODE` → pull の順で進む。
コードは60秒・一度限り。CLI は24時間の agent 認証を保存する。
対象1プロジェクトを pull し、カタログを更新する。全件巡回はしない。
人の認証ファイルはコピーしない。
詳しい判定は [connector.md](skills/kioku/references/connector.md) を参照する。

## CLI のバージョンとリリース

プラグインのバージョン `X.Y.Z` が、取得する CLI のバージョン `vX.Y.Z` を決める。
フックは `https://github.com/mash/kioku/releases/download/vX.Y.Z/` から `SHA256SUMS.txt` とアーカイブを取得する。
`SHA256SUMS.txt` にアーカイブの行がちょうど1つあり、その値が小文字の16進数64文字で、アーカイブの SHA-256 と一致するときだけ CLI を差し替える。
リリースは次の順に進める（[RELEASES.md](../docs/RELEASES.md)）。

1. `.claude-plugin/plugin.json` の `version` を上げる commit を作る。repo の `release` スキルが、ローカルのブランチ `release/vX.Y.Z` で commit を作り、`main` にマージする。
2. `main` を push すると、`release.yml` が tag `vX.Y.Z` を作り、リリースを公開する。

スキルやプロンプトの文だけを変えた場合も、ユーザーに届けるにはバージョンを上げる。

`KIOKU_CLI_DOWNLOAD_BASE` はローカル fixture 用のテスト専用設定とする。

macOS の初回確認（`<path>` はインストールした `kioku`）: `xattr -l <path>`、`<path> version`。

## Codex / プラグインなし

```sh
mkdir -p ~/.agents/skills
cp -R skills/kioku ~/.agents/skills/
```

既存ファイルがあれば差分を確認する。
`rules/kioku.md` を読み、必要な自動記録規則を AGENTS.md に統合する。
スキルの「プラグインなし」に従い、PATH の `kioku` をコマンド名に使う。
対象選択、認証、pull、カタログはプラグインと同じ手順を使う。

## 検証

```sh
sh -n hooks/kioku.sh
python3 -m unittest discover -s tests
KIOKU_TEST_BINARY=/absolute/path/to/kioku python3 -m unittest discover -s tests
```

旧コマンド・互換エイリアスは用意しない。
プラグインは自分と同じバージョンの CLI を使う。

GitHub 資料の取得はプロジェクト owner の読み取り専用 GitHub App 接続を使う。
プラグインは App の秘密鍵・user token を受け取らない。失敗を解消する場合は
対象ディレクトリの接続先で owner が `kioku connect github` を完了し、元 URL
を再投入する。member の接続で代用せず、remote MCP には App 管理を追加しない。
運用者の登録設定と移行は [GitHub App](../docs/GITHUB_APP.md) を参照。
