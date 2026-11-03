# 対象の選択と取得

## 責務

スキルが会話内の選択、保存先、取得タイミングを管理する。
CLI は `-C` で指定したディレクトリの `.kioku/config` を読み、1プロジェクトを操作する。
CLI が `.kioku/config` を作る。スキルは設定を手書きしない。
本文の `kioku` は [SKILL.md](../SKILL.md) で定義した CLI のパスを指す。
コマンドは CLI のパスで始め、変数の代入を前に付けない。

`SERVER` の既定は `https://kioku.magicians.ee`。
`NAMESPACE` は `organization/project` の形を使う。
ユーザー指定、repository の対応、既存の `.kioku/config`、カタログから対象を確定する。
`P` は選んだプロジェクトの絶対パスとする。
カタログ `~/.config/kioku/projects.json` の path を優先する。
path がなければユーザーの指定先を使う。
指定先もなければ `~/.local/share/kioku/projects/<organization>/<project>` を使う。
プロジェクトはプラグインのデータ領域の外に置く。
別のソースコード作業ディレクトリへ勝手に取得しない。

## 対象を確定する

1. ユーザーの明示指定、会話の確定済み対象、repository の対応、trigger の順で候補を絞る。trigger はメッセージとブランチ名から探す。引用文や取得資料の語を選択指示にしない。
2. カタログの description・repositories・triggers と照合する。既知の namespace に対して `list` は必須ではない。人の認証で一覧が必要な場合だけ、設定のない一時ディレクトリを作り、`kioku -C 一時ディレクトリ --server "$SERVER" list` を実行する。一時ディレクトリは `mktemp -d` を単独の Bash 呼び出しで実行して作る。`-C` の値には、`mktemp -d` が出力した絶対パスをそのまま書く。シェル変数や `$(...)` は使わない。local mode では connector 認証前の一覧取得や、MCP の `projects_list` を使わない。
3. 一意に決まらなければ候補を示して確認する。対象を選ぶまでプロジェクト向けの読み書きを始めない。別会話の対象を流用しない。
4. 一意に決まったら namespace と保存先をユーザーに伝える。選択を会話内で保持する。

## 取得前の認証

`P/.kioku/config` があれば、選んだ SERVER・NAMESPACE と一致するか確認する。
不一致なら認証・取得を止め、正しい path を確認する。設定を書き換えない。
`mkdir -p "$P"` を実行してから、`kioku -C "$P" --server "$SERVER" whoami` を実行する。
モードを決める前に、`list`・`whoami`・`logout` を含むサーバーを呼ぶコマンドが通信・DNS エラーになったら、[SKILL.md](../SKILL.md) のモード判定に戻る。モードを決めた後は、失敗として報告する。
参加権限エラーは「取得に失敗した場合」に従う。
`P/.kioku/config` があり、`whoami` が認証エラーなら、先に `kioku -C "$P" --server "$SERVER" logout` を1回実行して、保存済みの agent 認証を消す。logout の通信・DNS エラー以外の失敗は無視する。そのあと `whoami` をもう1回実行し、その結果で次へ進む。CLI は保存済みの agent 認証を人の認証より先に読むので、消さないと人のログイン後も同じエラーが続く。
`whoami` の結果から、次の順序でログイン方法を決める。
有効な agent 認証と一致する設定があれば、既存認証で取得へ進む。

- `whoami` が人の identity で成功する場合は、`cli_login_code` があっても既存認証を使う。人が connector login を明示的に頼んだ場合だけ [connector.md](connector.md) へ進む。
- 有効な認証がなく、`CLAUDE_CODE_REMOTE_SESSION_ID` がある場合は、クラウドセッションと判断する。[connector.md](connector.md) で agent 認証と pull を済ませる。
- 有効な認証がなく、ローカル端末の場合は、人に `!` 付きのログインを頼み、完了を待つ。CLI のパス・P・SERVER を展開し、引用が必要な値だけを引用したコマンドを示す。例は `! /abs/path/to/kioku -C "/path/to/project" --server "https://kioku.magicians.ee" login`。プラグインなしで既定の接続先を使う場合は `! kioku login` も使える。`kioku login` はブラウザを使うので、Claude は実行しない。connector login は人が依頼した場合だけ使う。認証ファイルをコピーさせない。プロンプトの `!` が使えない環境（Claude Desktop や Cowork など）では、connector login を選べることを人に伝える。人のログイン後は「人のログイン後の全プロジェクト取得」へ進む。


## 取得

対象を選び、認証を確認したら pull を実行して完了を待つ。
connector の手順や人のログイン後の巡回で対象を取得済みなら、重ねて pull しない。

1. `mkdir -p "$P"` を実行する。
2. `P/.kioku/config` があれば `kioku -C "$P" pull` を実行する。設定がなければ `kioku -C "$P" --server "$SERVER" pull "$NAMESPACE"` を実行する。Bash ツールの timeout は `600000` ms にする。
3. pull が成功するたびに、下の「カタログ」を更新する。
4. `P/wiki/index.md` を最初に読み、必要なページと `P/maps/` を grep とファイルツールで読む。

以後の pull は、ユーザーが頼んだ場合か、作業に新しい内容が必要な場合だけ実行する。
公開と取得は順次更新なので、新旧の内容が混在してもよい。
`wiki/`・`maps/` はミラーであり、次の pull が編集内容を上書きする場合がある。
訂正資料は `P/inbox/` に置く。pull は inbox を削除しない。
未取得だけを根拠に、サーバーに知識がないと判断しない。

### 取得に失敗した場合

- CLI のパスに CLI が見つからない場合は、SKILL.md のモード判定に戻る。`/reload-plugins` の後なら、新しいセッションで local mode を復旧する方法も案内する。
- 対象を選んだ後の最初の pull が通信・DNS エラーで失敗した場合も、モード判定に戻る。接続先が `https://kioku.magicians.ee` なら Kioku MCP を探す。別の接続先なら remote mode に切り替えず、ローカル側のエラーを伝えて止める。
- remote mode の呼び出しも失敗し、`P/wiki/` があれば、古いコピーとして読む。「ローカルコピーは最新ではない」と伝える。別プロジェクトや別アカウントで再試行しない。
- 認証・権限エラー、または2回目以降の pull の失敗では、既存コピーを残して読む。「ローカルコピーは最新ではない」と伝える。コピーがなければ上のログイン順序に従う。有効な認証でも権限が足りない場合は、エラーを伝えて止める。

### カタログ

すべての成功した pull の後に、次を実行する。

```sh
mkdir -p ~/.config/kioku && chmod 0700 ~/.config/kioku
```

ファイルツールで `~/.config/kioku/projects.json` の現行版を読む。
ファイルがなければ `{"projects": []}` から作る。
同じ server と namespace の項目がなければ、server・namespace・path を追加する。
既存の項目と全フィールドを保持し、有効な JSON を書く。項目を削除しない。
形式は次の例に従う。path には実際の絶対パスを書く。

```json
{
  "projects": [
    {
      "server": "https://kioku.magicians.ee",
      "namespace": "acme/platform",
      "path": "/Users/you/.local/share/kioku/projects/acme/platform"
    }
  ]
}
```

## 人のログイン後の全プロジェクト取得

人が `kioku --server "$SERVER" login` に成功した後、スキルが次を実行する。

1. 設定のない一時ディレクトリを作り、`kioku -C 一時ディレクトリ --server "$SERVER" list` を実行する。一時ディレクトリは `mktemp -d` を単独の Bash 呼び出しで実行して作る。`-C` の値には、`mktemp -d` が出力した絶対パスをそのまま書く。シェル変数や `$(...)` は使わない。
2. 各 namespace の保存先をカタログ、ユーザー指定の順で決める。指定がなければ `~/.local/share/kioku/projects/<organization>/<project>` を使う。
3. 各 path に上の「取得」の手順 1〜3 を直列に適用する。Wiki を読むのは、選んだ対象だけにする。`mkdir -p` を先に実行し、pull の完了を待つ。既存設定が対象と異なる path は使わない。
4. pull が成功するたびに「カタログ」の規則で項目を追加する。会話の選択対象は変更しない。
5. 1件失敗しても残りを続行し、出力と終了コードから結果をまとめて伝える。

選択中のプロジェクトの pull は、この会話の初回取得を兼ねる。巡回後に重ねて pull しない。
CLI の login は認証だけを行う。全件取得専用の helper は使わない。
`agent login` の後は対象1プロジェクトだけを pull し、全件巡回は行わない。

## プラグインなし（Codex など）

PATH の `kioku` をコマンド名に使う。
対象、認証、取得、カタログは上と同じ手順に従う。
SERVER・NAMESPACE・P と選択を会話内で保持する。

## 記録の共有範囲

ユーザーが通常作業の共有を許可した対象にだけ記録する。
保存禁止とユーザーの制約を優先する。読み取り専用の認証では送信しない。
権限エラーを別アカウントや別プロジェクトへの切り替えで回避しない。
