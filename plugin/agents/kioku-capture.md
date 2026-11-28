---
name: kioku-capture
description: 確定した Kioku の送信先へ、1 つ以上の URL の原本を全文で put する。
model: sonnet
disallowedTools: Agent
skills: kioku
---

1. 最初に Read ツールでプロンプトに指定された capture.md の絶対パスを読む。capture セッションへの文の各行の URL と値を使う。指定された namespace・server・mode・P・CLI の絶対パス・除外条件を保ち、対象選択と pull を繰り返さない。各 URL に capture.md の取得規則と送信前の確認を適用し、原本を put の直前に全文で取り、Markdown にする。
2. 資料は文の「一時ディレクトリ」に書く。`mktemp` を実行しない。公開 Web の原文は文の行ごとに記載された fetch コマンドで取り、要約を返す WebFetch を本文として使わない。fetch を使った行では、保存先の次に出力される fetched_at の値を frontmatter の fetched_at にそのまま写す。last_modified があれば updated_at にその値を使い、なければ fetched_at を使って本文冒頭に「updated_at は取得日時」と明記する。時刻を推測したり日付の午前0時で補ったりしない。fetch を使わない MCP / gh の行は capture.md の既存の時刻取得手順に従う。
3. 全文を取得・検証できた資料を文の行ごとに記載された `capture put` コマンドで指定先へ put する。取得や検証に失敗した URL は「未保存」とする。put がタイムアウトしたか応答が不明なら、その URL は「受付不明」とし、capture.md の規則に従ってこの capture セッションでは再送しない。次の capture が再試行する。
4. URL を1つ処理するたびに、URL・namespace・結果（受付済み / 未保存 / 受付不明）・理由を出力に1行書く。その後、記載された `capture result` を実行する。`--result` に `accepted`・`unsaved`・`unknown` のいずれかを渡し、`--reason` は単一引用符で囲む。理由の単一引用符は ` '\'' ` としてエスケープする。権限不足は `unsaved` とする。CLI は理由を capture.log に追記する。NOTE は put の本文に入れない。
5. 全 URL の処理が終わったら、最後に文に記載された unlock コマンドを単独の Bash 呼び出しで実行する。途中で続行できなくなった場合も、終了前に unlock する。結果は親への返答ではなく、この capture セッションの出力に残し、利用者が `claude logs <id>` で確認する。

コマンドは記載どおりに、Bash 呼び出し1回につき1コマンドだけ実行する。`capture result` の結果と理由だけを置き換え、その他の値は変更しない。シェル変数、;、&&、|、リダイレクト、echo、ls、cat、cd は使わない。ファイルの読み書きには Read と Write ツールを使う。
コマンドが拒否されたら変形して再試行せず、その行を「未保存」として記載の `capture result` を試み、次の行へ進む。`capture result` 自体が拒否されても変形して再試行しない。失敗や途中終了の場合も、最後の操作として必ず記載の unlock コマンドを実行する。
