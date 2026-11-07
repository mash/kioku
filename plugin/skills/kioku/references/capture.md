# 原資料の投入

remote mode では [remote.md](remote.md) の読み書き手順を使う。
本文の `kioku` は [SKILL.md](../SKILL.md) で定義した CLI のパスを指す。CLI の呼び出しは `kioku -C "$P"` で始める。

重要文書を参照・作成した時、結論を変えた文書を確定した時に、選択したプロジェクトへ原資料を投入する。ユーザーが通常のチーム作業として共有を許可した範囲で行い、機密・別チームの資料・保存禁止を除く。共有先が曖昧なら投入せず確認する。既存の `auto_put:false` などの自動送信禁止を尊重する。

投入するのは原文だけ。操作の要約、イベントID、参照したという記録、取得失敗のメタデータを別の資料にして put しない。原本 URL がなければ捏造せず、投入を保留して未保存と報告する。URL を得るために原本を無断で公開しない。

## 投入

1. GitHub、Google Docs、公開 Web など Kioku が取得できる資料は `kioku -C "$P" put URL` で URL を渡す。人の認証と agent 認証で手順を分けない。取得は生成時にプロジェクトオーナーの権限で行う。GitHub は owner が接続した読み取り専用 App の installation と現在の閲覧範囲で取得する。未接続・installation 外は未認証で取得し、既存接続の停止・認可障害では取得を止める。
2. Artifact など取得できない資料は、許可されたコネクターや画面で本文を読み、UTF-8 のファイルにそのまま書く。原本の HTTPS URL を YAML frontmatter の `source_url` に入れ、`kioku -C "$P" put FILE` でそのファイルだけを渡す。1ファイルなら `kioku -C "$P" put --source-url URL FILE` でもよい。FILE は絶対パスとする。引数なしの put で無関係な inbox を送らない。
3. git リポジトリ内の文書には、そのファイルを指す URL（例: `https://github.com/org/repo/blob/main/docs/foo.md`）を使う。リポジトリのトップ URL を source_url にしない。
4. 応答の inbox ID と status（URL は pending、本文は fetched）を確認し、資料を受け付けたことだけを報告する。取得や Wiki 更新の成功とは言わない。1要求の JSON 全体は1 MiBまで。複数ファイルは CLI が1件ずつ送るため、途中失敗時は成功した項目と失敗した項目を分けて報告する。

本文を読めない場合は、タイトルから要約を作ったりログイン画面を原文と扱ったりしない。認証情報・署名 URL の秘密部分は保存しない。一時ファイルは受け渡しにだけ使い、永続ローカル台帳を作らない。同じ会話で同じ文書の同じ版を二重投入しない。字句修正、日常のコマンド出力、生成 Wiki の再読は投入しない。

原資料を取り除く依頼には `kioku -C "$P" delete URL` を使う。removed の項目を受け付けた後、次の生成で source ページと参照を除く。待機中の同じ URL は新しい put で置き換えるが、実行中の項目はその実行に残る。

## 失敗と鮮度

読み取り専用なら put／delete を実行しない。権限不足・オフラインでは未保存と伝え、別アカウントや別プロジェクトに切り替えない。受付成否が不明なら盲目的に再送せず、`kioku -C "$P" status` の inbox を確認する。項目が見つからないだけでは、未受付か既に生成済みかを区別できないので、確認できないことを報告する。

put は受付で返り、取得・生成を待たない。Wiki は定期生成か、即時反映の依頼時の `kioku -C "$P" generate --wait` で更新する。後者は生成完了後に pull する。取得の失敗は Runner が `wiki/log.md` に書く。利用者が尋ねたときに log.md を読んで答える。失敗の記録を別途 put しない。

## GitHub の取得失敗を解消する場合

取得失敗を尋ねられたときは対象プロジェクトの `wiki/log.md` を確認する。
接続や再取得を依頼されたら、選択済みディレクトリと接続先を維持し、
**プロジェクトの owner 本人**に `kioku -C "$P" account status` と
`kioku -C "$P" connect github` を案内する。App の利用者認可、installation
選択、同じ CLI での完了が必要。member の接続や別人の token で代用しない。
configured=false / enabled=false は運用者へ、reauthorization_required は owner
の再接続へ、suspended / inaccessible は GitHub 側の停止・repo 選択・現在の
閲覧権限の確認へ案内する。組織の承認待ちは管理者の承認後に接続をやり直す。
404 だけで非公開 repo が存在しないと断定しない。接続情報は最終確認時点の状態。

解消後、再取得を依頼された元の URL を `kioku -C "$P" put URL` で再投入する。
完了済み生成の inbox は消費されており、connect だけでは再取得されない。
即時反映を依頼された場合だけ `generate --wait`、完了後に `pull` を行う。
失敗メタデータは put しない。権限は Contents・Metadata・Issues の read のみで、
PR 差分やレビューの取得のために権限を増やさない。remote mode では App 管理を
MCP へ追加・推測せず、owner のローカル CLI での作業を案内する。
