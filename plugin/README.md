# Kioku plugin for Claude

会話の対象と取得タイミングを管理するプラグイン。
CLI（コマンドラインの操作ツール）が保存先を選び、1プロジェクトを操作する。
CLI は接続先と namespace（organization/project 形式のプロジェクト名）を `.kioku/config` に置き、保存先を `~/.config/kioku/projects.json` に記録する。

## 責務

| スキル | CLI |
| --- | --- |
| 会話内のプロジェクト選択 | 接続先別の認証 |
| projects.json の参照と説明案の確認 | 保存先の選択・作成、`.kioku/config` と projects.json の読み書き |
| 対象選択直後の pull、人のログイン後の巡回 | 単一プロジェクトの差分取得・再試行 |
| Wiki の参照と結果の説明 | ファイル反映・取得メタデータ・更新ロック |

## CLI の操作

プラグインは `${CLAUDE_PLUGIN_DATA}/bin/kioku` の CLI を使う。
以下の `kioku` はそのパスに置き換える。プラグインなしでは PATH の CLI を使う。
`NAMESPACE` は対象のプロジェクト名、`SERVER` は接続先を指す。

```sh
kioku -n "$NAMESPACE" pull        # CLI が保存先を用意して紐づけ、Wiki を取得
kioku -n "$NAMESPACE" status   # どの作業ディレクトリからでも同じプロジェクトを参照
```

紐づけとは、CLI が保存先の `.kioku/config` に接続先と namespace を書き、そのディレクトリを1プロジェクトに結ぶことを指す。
`-n` は接続先を `--server`、`KIOKU_SERVER`、既定の `https://kioku.magicians.ee` の順で決める。
CLI は projects.json の同じ server・namespace の path を使う。項目がなければ `~/.local/share/kioku/projects/<organization>/<project>` を使い、必要なディレクトリを作る。
既存の紐づけが対象と異なる場合、CLI は設定を書き換えずに止まる。

新規作成は `kioku -n "$NAMESPACE" project create --description "プロジェクトの説明"` を使う。
ユーザーが保存先を明示した場合だけ、既存ディレクトリに対して `kioku -C DIR -n "$NAMESPACE" pull` を使う。
create は `-n` を必須とし、pull と create は namespace の位置引数を受け付けない。
`-n` を省略すると、CLI は cwd（作業ディレクトリ）か `-C DIR` の `.kioku/config` を使う。
`-C DIR -n "$NAMESPACE"` は DIR を使い、未紐づけか同じ接続先・namespace への紐づけを必要とする。
`project join INVITE_URL` は cwd または `-C DIR` へ紐づけ、`-n` を受け付けない。
CLI は親ディレクトリを探索しない。

`wiki/` は保存先の直下、CLI のメタデータは `.kioku/` 内に置く。
CLI はユーザー共通領域で接続先別に認証を管理する。スキルは認証情報を読まない。

## 原資料の送信

Claude Code の capture セッションは文で指定された一時ディレクトリを使う。その他の環境で送信する原資料は `mktemp -d` で作った一時ディレクトリに Markdown として書き、`kioku -n "$NAMESPACE" put FILE` で送信する。`mktemp -d` は単独の Bash 呼び出しで実行し、出力した絶対パスをそのまま使う。シェル変数や `$(...)` は使わない。

ユーザー自身の Claude が、コネクター、CLI、手元のファイル、ユーザーが貼った本文から原文を読む。
資料の種類を問わず、全文を Markdown にする。
原本の HTTPS（暗号化した Web 通信）URL（資料のアドレス）と本文を送信する。
サーバーは原資料を取得しない。操作の要約やイベント記録は送信しない。

製品名ではなく、[SKILL.md](skills/kioku/SKILL.md) の機能・接続先判定でモードを選ぶ。
シェル、CLI、サーバー接続を使える local mode を優先する。
local mode は `.md` ファイルを CLI の `kioku -n "$NAMESPACE" put FILE` で送信する。
remote mode は MCP（Model Context Protocol）の `source_put` で本文を送信する。
remote mode の接続先は `https://kioku.magicians.ee` に限る。
`source_url`・`updated_at` を含む frontmatter と送信前の確認は [capture.md](skills/kioku/references/capture.md) に従う。
原本の HTTPS URL がない資料は送信せず、未保存と伝える。
`source_url` は送信者の申告であり、サーバーは本文と原本の一致を検証しない。

両モードとも操作の成功は Kioku がファイルを受け付けたことを示す。ユーザーに受付を伝え、Wiki の生成を待たない。
CLI はファイルごとに `accepted <path>`、削除の依頼ごとに `removed <SOURCE_URL>` を表示する。
local mode の削除は `kioku -n "$NAMESPACE" delete URL`、即時生成は `kioku -n "$NAMESPACE" generate --wait` を使う。
remote mode の削除は `source_put` の `remove` を使い、Wiki は最後の put の約 10 秒後の自動生成を待つ。
原本が変わった時は Claude が本文を読み直して送信する。
同じ `source_url` では、サーバーは後から受け付けた本文を使う。
サーバーは版を比較しないため、古い版でも新しい版を上書きする。

## Claude Code

Claude Code 2.1.322 以上を使う。それ未満の動作は保証しない。
Claude Chat と Cowork は hook と plugin の agent を持たないため、今の skill の手順で動かす。

| 利用環境 | インストール | Kioku MCP の接続 | 初回の動作 |
| --- | --- | --- | --- |
| Claude Code | `/plugin marketplace add mash/kioku` の後に `/plugin install kioku@kioku`。開発時は `claude --plugin-dir /absolute/path/to/kioku/plugin` も使える。 | `/mcp` | SessionStart が取得した CLI で local mode を使う。CLI やサーバーを使えなければ、モード判定に従う。 |
| Claude Desktop / Cowork | Customize > Plugins > Add marketplace に `mash/kioku` を追加し、`kioku` をインストールする。 | プラグインの Connectors タブ | スキルがモードを選ぶ。Cowork のネットワーク接続は未検証。 |
| claude.ai chat | 同じ marketplace からインストールする。 | プラグインの Connectors タブ | スキルが機能と接続先からモードを選ぶ。local mode を使えなければ MCP を確認する。 |

claude.ai アカウントのプラグインは Claude Code にも同期する（[公式仕様](https://claude.com/docs/plugins/platform-support.md)）。

プロンプト本文はリポジトリルートの [`prompts/`](../prompts/) に集約する。
`plugin/prompts` は `prompts/` へのシンボリックリンクとする。
marketplace からのインストール処理は、リンク先の内容をプラグイン内へコピーする。
プラグイン単体を配布するときは、`cp -RL plugin /path/to/package` などでリンクを実体化する。

SessionStart フックは POSIX（移植可能なシステム操作の規格）の sh を使う。Python は不要。
CLI のパスは `${CLAUDE_PLUGIN_DATA}/bin/kioku` の1つだけで、実体の実行ファイルを置く。
startup・clear・fork では、フックが `bin/kioku version` を実行し、次の場合だけプラグインと同じバージョンの CLI を取得する。

- `bin/kioku` がない、実行できない、または `version` が失敗する。
- 入っている CLI のバージョンの文字列が、`.claude-plugin/plugin.json` の `version` に `v` を付けた値と異なる。古くても新しくても取得する。

同じバージョンなら、フックは何もしない。フックはバージョンの新旧を比べない。
compact・resume ではフックが CLI を取得せず、会話の状態ファイルから選択を復元する。
保存済みの対象と最後の pull の時刻・結果を1行で `hookSpecificOutput.additionalContext` に入れる。
状態ファイルが無い・壊れている・mtime から30日を超えた場合は、今の hook-compact.txt を出す。
古いプラグインのセッションも compact でフックを再実行するので、この規則で古いバージョンへ戻る主な経路をなくす。
ただし古いセッションでプラグインの更新後に `/clear` を実行すると、古いバージョンの CLI を一度取得することがある。
次の新しいセッションが、現在のバージョンの CLI を再び取得する。
フックは SHA-256（256ビットのハッシュ関数）でファイルを検証し、一時ディレクトリに置く。
1回の `mv -f` で `bin/kioku` へ差し替える。
既存のファイルには書き込まないので、実行中のプロセスは古いファイルを使い続ける。
プラグインの更新前に始めたセッションも、次のコマンドから新しい CLI を使う。
コマンドの後方互換は提供しない。このリリースは CLI 0.1.0 以上を要求する。
サーバーは古い版や版ヘッダーのない API（プログラム間の操作窓口）要求を426で拒否する。
更新後は新しいセッションで現在のスキルと CLI を使う。

初回取得には curl、tar、sha256sum または shasum が必要。
対応環境は macOS と Linux の amd64・arm64。
CLI の取得には `github.com` と転送先へのネットワーク接続が必要になる。
転送先は `release-assets.githubusercontent.com` または `objects.githubusercontent.com` とする。
取得失敗はセッションを止めない。古い CLI が動けばそれを使い、次の startup・clear・fork が再度取得を試みる。
`plugin.json` の `version` が `X.Y.Z` の形でなければ、フックは取得しない。動く `bin/kioku` がなければ、CLI が無いことを伝える文を出す。
そのバージョンのリリースがまだ無いときも取得に失敗し、フックは今の CLI を使い続ける。

スキルは CLI のパスをそのままコマンド名に書き、変数や `cd` を前に付けない。

開発時は Claude Code の起動前に `KIOKU_CLI=/abs/path/to/kioku` を設定する。
startup・clear・fork では、フックが取得とバージョン確認を省き、そのファイルを `bin/kioku` へコピーする。
コピーも同じ一時ファイルと `mv -f` を使う。compact・resume ではコピーしない。
スキルは `KIOKU_CLI` を読まず、常に `bin/kioku` を使う。
ローカルでビルドした CLI のバージョン（`git describe` の値や `dev`）はプラグインと異なる。
`KIOKU_CLI` を外すと、次の startup がプラグインと同じバージョンの CLI を取得する。
通常のプラグインは PATH の `kioku` を使わない。
`/reload-plugins` は CLI を取得しない。新しいバージョンの CLI は次の startup などが取得する。

フックは SessionStart・PreCompact・UserPromptSubmit・Stop・SessionEnd で動く。
PreCompact は namespace・server・mode・P・最後の pull の時刻と結果を要約に残すよう指示する。
compact・resume は保存済みの選択を復元し、Wiki の本文は再注入しない。
UserPromptSubmit・Stop・SessionEnd は `agent_id` がある入力では何も行わない。

UserPromptSubmit は未選択時に、projects.json の trigger 語をプロンプトと cwd のブランチ名に照合する。
ASCII だけの語は大文字小文字を区別せず、前後が `[a-z0-9]` でない位置に一致する。その他は部分一致で、ASCII 部分の大文字小文字を区別しない。
Git を起動せず `.git/HEAD` を読み、worktree の `.git` ファイルにも対応する。detached HEAD や Git の外ではブランチ名を使わない。
1 project だけに当たり、プロンプトに別の有効な namespace がなければ、hook が選択を保存し、背景で `kioku --server SERVER -n NAMESPACE pull` を起動する。
手元コピーの index.md・overview.md と選択理由を渡し、pull の完了を待たずに戻る。コピーがなければ背景で取得中と伝える。
別の namespace が projects.json に無くても、自動選択しない。
背景 pull は、その完了時も server・namespace・path が同じ場合だけ結果を保存する。

自動選択できない場合は、手元の全 project の namespace・description を一覧で出す。
同じ server・namespace を1件にまとめ、trigger 語に当たった project を先頭へ並べる。namespace 完全一致と最寄りの `.kioku/config` はヒントとして表示する。
既定以外の server も表示する。projects.json が無い・空・不正なら、最寄りの設定をヒントとして出す。候補がなくても General の選択手順を表示する。
親は最初のメッセージで namespace の指定があれば、projects.json に無くても優先する。指定が無ければ description で対象を選ぶ。対象が不明なら、`kioku project list` の `"general": true` の項目から namespace を使う。選択には `kioku -n NAMESPACE session select` を使う。
選択済みなら trigger 語を照合せず、毎ターン index.md の場所と queue add の規則を出す。背景 pull 中と失敗の文を区別する。

`session select` は通常の pull と同じ認証・保存先を使い、成功時に選択と結果を保存する。
stdout は Wiki の入口と冒頭の1行の指示だけを返し、通常の pull の JSON や「最新です」は出さない。
pull に失敗しても実行前の手元コピーがあれば、そのコピーと警告を返して failure を保存する。コピーがなければ選択を変えずに失敗する。
入口の読み取りは1ファイル40 KiBまで、出力は指示を含め10,000文字までとし、overview.md、index.md の順で末尾を省略する。
選択したターンは「Kioku: NAMESPACE の wiki を読み込みました（理由: …）」を返答の冒頭に1行書く。
利用者の訂正には別の `session select` で対応する。`session list` は description と trigger 語の全件を表示する。

CLI は `CLAUDE_CODE_SESSION_ID` がある場合、選択を `${XDG_STATE_HOME:-~/.local/state}/kioku/sessions/<sid>.json` に保存する。
選択時は `server`・`namespace`・`mode`・`path` を残し、pull 後は `last_pull_at`・`last_pull_result` を残す。
pull・create・join が成功すると、そのプロジェクトを選択する。通常の pull の失敗は同じ対象の結果だけを更新し、create・join の失敗は選択を変えない。
状態ファイルは0600、ディレクトリは0700、期限は mtime から30日。CLI が書いた後に期限切れを削除する。
hook が状態を書くのは自動選択時だけ。状態ファイルを送信先や認証・共有権限の正本にしない。

Claude Code の local mode では、親は共有範囲・機密・保存禁止を確認し、重要な文書の URL を `kioku -C P -n NAMESPACE queue add URL "何をしたか"` で記録する。
`P/.kioku/queue.tsv` は日付・HTTPS URL・NOTE の3列、0600。同じ URL の再追加は日付と NOTE を更新して末尾へ移す。`queue list` で読み、`queue remove URL...` で消す。
キューはセッションをまたいで project ごとに共有する。NOTE は put 本文に入れない。Codex・Claude Chat・Cowork と remote mode の直接 put は変えない。

Stop は5件以上、SessionEnd は1件以上で、ロックが無いか30分を超えて古い時だけ背景の `capture run --min N` を起動する。hook は出力せず、子を待たない。
`capture run` の既定の閾値は5件。今すぐ送る依頼では [SKILL.md](skills/kioku/SKILL.md) の「今すぐ送る」手順に従う。
子は `P/.kioku/capture.lock` を排他作成し、信頼済みの作業 cwd で絶対パスの `claude --bg` を起動する。文は位置引数で渡し、`-p` は使わない。
この capture セッションは hook 無効・dontAsk・指定したツール許可で Sonnet の `kioku-capture` を使う。資料は P/.kioku/capture-tmp/<token> に書き、所有トークンの一致する unlock が削除する。次の run は稼働ロック以外の一時ディレクトリを削除する。
URL ごとに全文を取得・検証・put し、受付済み・未保存・受付不明の結果を出力して queue remove する。最後に所有トークンを付けて capture unlock する。処理中に追加された URL は次の run が拾う。

Capture は開始時のキューの日時・URL・注記をスナップショットに保存します。`queue remove --capture-token TOKEN URL` は現在の行がそのスナップショットと同じ場合だけ削除し、後から日付や注記を変えて再追加した行は次回に残します。利用者の `queue remove URL` は従来どおり URL に一致する行を削除します。
起動失敗はロックを消してキューを残す。id が読めない場合は警告と capture.log に残してロックを保持し、前回の片付けは行わない。
ロックは pid の生死ではなく30分の期限で判定する。unlock は所有トークンが一致した時だけ bg_id を `capture.last` に保存してロックを消す。
capture セッションは idle で残り、次の run が capture.last の完了済み bg_id だけに `claude rm` を試す。期限切れロックの所有セッションは削除しない。結果は `claude agents` と `claude logs <id>` で確認する。
背景 pull の出力は `pull.log`、背景 capture run の出力は `capture.log` に保存する。両方とも0600で、hook が子を起動するたびに内容を切り詰めて上書きする。capture の処理中の記録は capture.log に追記する。`capture.last` も0600。

sh は JSON の解析を `session hook --event EVENT --prompts DIR` に任せる。CLI の hook は直接通信せず、必要な子だけを起動する。jq と Python は不要。
不正 JSON・不正 SID・8 MiB超の入力は無出力で終了する。sh は stderr を捨て、失敗してもセッションを止めない。
追加の pull は、ユーザーの依頼か、作業に新しい内容が必要な場合だけ実行する。既存コピーを最新と呼ばない。
Claude Code のサンドボックスを使う場合は、`kioku.magicians.ee` への通信を設定で許す。
`~/.local/share/kioku/projects`・`~/.config/kioku`・`${XDG_STATE_HOME:-~/.local/state}/kioku/sessions` への書き込みも許す。
既定以外のプロジェクトの path にも書き込みを許す。権限が足りないと pull が失敗する。

選択を会話と要約に残し、Claude Code の CLI は状態ファイルにも最後の書き込みから30日まで置く。
スキルは projects.json を読むだけにする。
CLI は create・join・pull・agent login の紐づけ成功時と、毎回の pull で項目を足す。project delete の成功時は項目を消す。
既存の path は変えない。別の path へ紐づけた場合は既存 path を stderr（標準エラー出力）に示す。
書き込みだけに失敗した場合は終了コード0と警告を返す。登録失敗には次の pull、削除失敗にはファイルの修復と該当項目の削除を案内する。壊れた JSON（データ交換形式）はファイル名を示して止まる。
[examples/projects.json](examples/projects.json) に形式の例がある。

description は案件・主な語彙・対象 repo を1〜3文、200文字以内を目安に書く。選んだ project の説明が足りなければ、親は返答の末尾に1行の案を示す。新規作成時も説明案を確認する。
確認後に create・join・pull の `--description` で渡す。スキルは projects.json を手書きしない。
trigger 語は利用者が求めた時だけ `project triggers WORD...` で置き換え、語なしで消す。重複を除き、同じ server・namespace の全保存先に書く。通常の pull・create・join は既存の語を保つ。
プロジェクトはプラグインのデータ領域の外に置き、アンインストール後も inbox を残す。

人の `kioku login` が成功したら、スキルが `project list` を確認し、各 namespace で `kioku -n "$NAMESPACE" pull` を直列に実行する。
選んだ対象以外を先に pull し、選んだ対象を最後に pull する。説明保存の pull も同じ順序にする。
1件失敗しても残りを続行し、結果をまとめて伝える。選んだ対象が失敗しても、別の保存済み対象を変えない。
CLI の login 自体は取得しない。
選択中のプロジェクトの pull は会話の初回取得を兼ねる。重ねて pull しない。

## Connector login（ローカル・クラウド共通）

対象サーバーの `/mcp` を Claude の connector に接続する。
既定は `https://kioku.magicians.ee/mcp`。
local mode の namespace はユーザー指定、作業ディレクトリの `.kioku/config`、projects.json の description から確定する。
認証前の `project list` は不要。

スキルは `whoami` の `agent_id` を確認する。人の認証が有効なら使い続ける。
人の認証がなく、有効な agent 認証があれば、agent 認証を使う。
有効な認証がないクラウドセッションでは connector login を使う。
ローカル端末では人に `!` 付きのログインを頼み、完了を待つ。Claude は login を実行しない。
`!` が使えない環境（Claude Desktop や Cowork など）では connector login を使う。
人の認証がある状態で人が connector login を頼んだ場合は、`kioku logout` が人のセッションを終えることを説明し、同意を得てから logout する。

connector login は `cli_login_code` → `kioku -n "$NAMESPACE" agent login CODE` → pull の順で進む。
コードは60秒・一度限り。CLI は24時間の agent 認証を保存する。
対象1プロジェクトを pull する。CLI が projects.json に項目を足す。全件巡回はしない。
人の認証ファイルはコピーしない。
詳しい判定は [connector.md](skills/kioku/references/connector.md) を参照する。

## CLI のバージョンとリリース

プラグインのバージョン `X.Y.Z` が、取得する CLI のバージョン `vX.Y.Z` を決める。
フックは `https://github.com/mash/kioku/releases/download/vX.Y.Z/` から `SHA256SUMS.txt` とアーカイブを取得する。
`SHA256SUMS.txt` にアーカイブの行がちょうど1つあり、その値が小文字の16進数64文字で、アーカイブの SHA-256 と一致するときだけ CLI を差し替える。
リリースは次の順に進める（[RELEASES.md](../docs/RELEASES.md)）。

1. リポジトリの `release` スキルで `.claude-plugin/plugin.json` の `version` を上げる。
   スキルは `release/vX.Y.Z` で commit を作り、`main` にマージする。
2. `main` を push すると、`release.yml` が tag `vX.Y.Z` を作り、リリースを公開する。

スキルやプロンプトの文だけを変えた場合も、ユーザーに届けるにはバージョンを上げる。
`-n` を使うこのスキルは、対応する CLI のリリースと一緒に配布する。

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
対象選択、認証、pull、projects.json の参照はプラグインと同じ手順を使う。

## 検証

```sh
sh -n hooks/kioku.sh
python3 -m unittest discover -s tests
KIOKU_TEST_BINARY=/absolute/path/to/kioku python3 -m unittest discover -s tests
KIOKU_BIN=/absolute/path/to/kioku python3 -m pytest -q tests/test_hooks.py
```

旧コマンド・互換エイリアスは用意しない。
プラグインは自分と同じバージョンの CLI を使う。
