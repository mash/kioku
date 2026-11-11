# Kioku へのフィードバック

ユーザーの投稿依頼に応じて、公開リポジトリ `mash/kioku` に Issue を作る。実装相談だけでは投稿しない。Kioku の認証・namespace・原本 URL は不要。

- タイトルは `[Feedback] <問題の短い説明>`。本文は期待した動作・実際の動作・発生状況・分かる利用環境を簡潔にまとめる。本人報告、直接確認した事実、原因の仮説を区別し、不明な情報は補わない。
- 会話全文や認証情報、個人情報、非公開の namespace・資料 URL・Wiki 本文・ローカルパスを含めず一般化する。公開可能な説明にできない場合だけ確認する。通常の投稿依頼を再確認する必要はない。
- 本文を一時 Markdown ファイルへ書き、次のタイトル・絶対パスを実際の値に置き換える。作業中のリポジトリへ誤投稿しないよう `--repo` を必ず指定する。既存の `feedback` ラベルを付ける。

```sh
gh issue create --repo mash/kioku --label feedback --title '[Feedback] Wiki のリンクが付かなかった' --body-file /absolute/path/feedback.md
gh issue view <number> --repo mash/kioku --json url,title,body,labels
```

`gh` が使えなければ、利用可能な GitHub 接続の Issue 作成機能を使い、`feedback` ラベルを付ける。成功時は返された Issue URL を確認し、ユーザーにリンクを返す。
認証・権限不足や明確な失敗では未投稿と伝えて下書きを返す。作成結果が不明なら最近の自分の Issue を本文・時刻まで照合し、確認できるまで再投稿しない。作成成功後の読み取り失敗では URL と確認未完了を伝える。
他の Issue の変更や Kioku の `put` は行わない。一時ファイルは投稿後、失敗時は下書きを返した後に削除する。
