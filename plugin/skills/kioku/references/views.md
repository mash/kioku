# View の設定と生成

1. HTML の View は、プロンプトを書く前に `kioku view rules` を実行して出力規則を読む。許可された HTML 要素・属性、参照先、CSS、`index.html`、任意の `summary.md`・`data.json`、アセットとサイズの上限が出る。プロンプトには表示したい項目、Wiki の根拠、表示順、必要な任意ファイルを書く。まず数段落か短い箇条書きにまとめる。
2. 対象プロジェクトは通常の選択手順で確定し、`kioku -n NAMESPACE view list` で slug を確認する。`kioku -n NAMESPACE view show SLUG` で現在の設定と直近の生成状態を読む。
3. プロジェクト所有者のログインで、UTF-8 のプロンプトをファイルに保存し、`kioku -n NAMESPACE view set --prompt-file FILE SLUG` を実行する。既存 View のタイトルはそのまま使う。新規 View の作成は Web のプロジェクト画面から行う。予約 slug `wiki` は Wiki の Markdown の整理方法を設定する。HTML の出力規則はほかの View に適用する。
4. `kioku -n NAMESPACE generate --wait` でプロジェクトの生成を待つ。Wiki の処理後、残りの予算で View が作成順に生成される。既に Wiki があり、inbox が空でも通常の View は生成できる。Wiki のプロンプトは次に Wiki が生成されるときに使われる。失敗した View の後にある View はスキップされる。
5. 待機出力の View ごとの状態とエラーを確認する。後から確認する場合は `view show SLUG`、生成全体を確認する場合は `job status JOB_ID` を使う。検証違反なら表示されたファイルと要素・属性をプロンプトで見直す。予算によるスキップや停止と、時間切れ、生成出力の検証違反を状態・理由で区別する。

コマンド例の `kioku` はプラグインが指定する CLI の絶対パスに置き換える。リモートの読み取りは `view_list`・`view_read` を使う。設定は、所有者が Web の「View の設定」から保存できる。
