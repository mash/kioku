# Connector から CLI を認証して取得する

local mode では、確定した `SERVER`（接続先）・`NAMESPACE`（organization/project 形式のプロジェクト名）を使う。
CLI が保存先を選ぶ。ユーザーが保存先を明示した場合だけ [selection.md](selection.md) の `-C` の手順に従う。
Kioku の `cli_login_code` を使えるか確認する。
local mode では `projects_list` を使わない。
namespace はユーザー、作業ディレクトリの `.kioku/config`、projects.json の description（プロジェクトの説明）から決める。
本文の `kioku` は [SKILL.md](../SKILL.md) で定義した CLI のパスを指す。
コマンドは CLI のパスで始め、変数の代入を前に付けない。

## 認証と pull

通信・DNS エラーは [selection.md](selection.md) の「取得に失敗した場合」に従う。
参加権限エラーも同じ失敗規則に従い、対象やアカウントを変えない。

1. ツールの接続先が SERVER と一致するか確認する。CLI が紐づけの不一致を返した場合は止め、設定の上書きや別アカウントへの切り替えで回避しない。
2. `kioku -n "$NAMESPACE" whoami` を実行する。直前に同じコマンドを実行した場合は、直前の結果を使う（logout の後は実行し直す）。終了コードだけでなく JSON の **`agent_id`** を確認する。人の identity なら既存認証を使う。コード交換をせず、手順5へ進む。人が connector login を頼んだ場合は、[selection.md](selection.md) の規則で logout の同意を得てから、この手順をやり直す。設定があり、有効な agent identity を確認できれば既存認証を使う。
3. `whoami` が認証エラーで失敗する場合は、コードを取得する。
   `cli_login_code` に `project: NAMESPACE` を渡す。role は既定の `read`。
   依頼が本文の送信・資料の削除・生成を含む場合だけ `write` を指定する。
   自動記録のために read 認証を昇格しない。
4. ツールが返したコードを**直ちに**次の CLI コマンドの `CODE` に渡す。60秒・一度限りなので、他の調査を間に挟まない。

   ```sh
   kioku -n "$NAMESPACE" agent login CODE
   kioku -n "$NAMESPACE" whoami
   ```

   `agent login` が `a human login exists` のエラーで失敗した場合は、[selection.md](selection.md) の規則に従う。
   CLI は NAMESPACE を送って交換し、サーバーが別の namespace を返したら止まる。
   ログイン結果の namespace が期待する対象と一致するか確認する。
   再確認した `whoami` に `agent_id` があることも確認する。
   CLI が設定と24時間の agent 認証を保存する。
   コードを echo・ファイル保存・報告へ転記せず、認証ファイルを読まない。
   CLI が agent トークンを管理する。
5. 認証を確認できたら、[selection.md](selection.md) の「取得」に従って pull する。`kioku -n "$NAMESPACE" pull` を使う。Bash ツールの timeout は `600000` ms にし、完了を待つ。

   CLI が projects.json に項目を足す。スキルはその `path` を保存先の絶対パス P として読む。
   description がなければ selection.md の「説明の確認と新規作成」に従う。
   `P/wiki/index.md`、関連ページ、`P/maps/` を grep とファイルツールで読む。
   この pull は会話の初回取得を兼ねる。対象選択の手順に戻って重ねて pull しない。

## 失効・利用できない場合

- コードが期限切れ・使用済みなら、新しいコードを取り直して一度だけ再実行する。agent 認証が期限切れ・失効した場合（エラーに `run kioku agent login` を含む）は、新しいコードを取得して `agent login` をやり直す。logout は不要。同じ失敗が続く場合はエラーを報告して止める。
- `cli_login_code` が使えず、有効な agent 認証もない場合は、同じ SERVER の `/mcp` への接続を案内する。認証ファイルやトークンの貼り付けを求めない。
- 参加権限エラーは対象やアカウントの切り替えで回避しない。read 認証では書き込みを実行せず、未保存を報告する。人が書き込み権限の取得を明示的に頼んだ場合だけ、選択中の対象の write コードを取得する。
- pull の失敗後はローカルコピーを最新と呼ばない。認証を更新した後は、通常の pull を明示実行できる。
