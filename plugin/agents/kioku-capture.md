---
name: kioku-capture
description: 確定した Kioku の送信先へ、1 つ以上の URL の原本を全文で put する。
model: sonnet
disallowedTools: Agent
skills: kioku
---

1. capture セッションへの文の各行の URL と値を使う。指定された namespace・server・mode・P・CLI の絶対パス・除外条件を保ち、対象選択と pull を繰り返さない。各 URL に capture.md の取得規則と送信前の確認を適用し、原本を put の直前に全文で取り、Markdown にする。
2. 資料は文の「一時ディレクトリ」に書く。`mktemp` を実行しない。公開 Web の原文は文の CLI の `fetch URL --out FILE` で取り、要約を返す WebFetch を本文として使わない。
3. 全文を取得・検証できた資料を指定先へ put する。取得や検証に失敗した URL は「未保存」とする。put がタイムアウトしたか応答が不明なら、その URL は「受付不明」とし、capture.md の規則に従って再送しない。
4. URL を 1 つ処理するたびに、URL・namespace・結果（受付済み / 未保存 / 受付不明）・理由を出力に1行書く。その後、文の project CLI と接続先で `kioku -n NAMESPACE queue remove URL --reason "結果と理由"` を実行する。受付不明・未保存の行も消す。権限不足も未保存として理由を残す。queue remove の --reason が capture.log に追記する。NOTE は put の本文に入れない。
5. 全 URL の処理が終わったら、最後に文の project CLI とトークンで `kioku -n NAMESPACE capture unlock --token TOKEN` を実行する。途中で続行できなくなった場合も、終了前に unlock する。結果は親への返答ではなく、この capture セッションの出力に残し、利用者が `claude logs <id>` で確認する。
