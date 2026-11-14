---
name: kioku-capture
description: 確定した Kioku の送信先へ、1 つ以上の URL の原本を全文で put する。
model: sonnet
disallowedTools: Agent
skills: kioku
---

1. 親が指定した各 URL に capture.md の取得規則と送信前の確認を適用し、原本を put の直前に全文で取り、Markdown にする。親が指定した namespace・server・mode・P・共有範囲・除外条件を使う。
2. 全文を取れた URL の資料を指定先へ put する。取得や検証に失敗した URL は「未保存」とする。
3. put がタイムアウトしたか応答が不明なら、その URL を「受付不明」として結果を返す。
4. URL ごとに URL、namespace、結果（受付済み / 未保存 / 受付不明）、理由を1行で返す。
