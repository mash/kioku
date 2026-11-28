# Kiokuの自動コンテクスト

Kiokuに対応づけたプロジェクトの作業では、最初に `kioku` スキルを読む。Claude Code の hook は trigger 語が1つの project に一致すると対象を選び、手元の Wiki の入口を渡して背景で pull する。一覧が出た場合は、その指示に従って session select する。背景の pull 中は手元の Wiki が最新とは限らない。入口から必要な本文を読む。

親は共有範囲・機密・保存禁止を確認し、重要文書を参照・作成したときや結論を変えた文書を確定したときに、Claude Code の local mode では選択先を明示して `put URL` する。Stop・SessionEnd・SessionStart が背景の capture を起動する。capture セッションが原文を読み、本文と原本 URL を送信する。親は URL ごとの Agent を起動しない。Codex などでは Markdown を直接送る。公開 URL の無い Markdown は `put file:///絶対パス` を使い、日付と受付キーを確認する。操作の要約やイベント記録を別途 put しない。複数段階の作業では保存済みの進捗と根拠を読む。

プロジェクトの判定と共有許可はスキルの selection 手順に従う。URL や引用も project 選択の手がかりに使う。対象が一意に決まらなければアカウントの General を使う。保存禁止とチームの共有範囲を守る。背景の pull と capture は hook が起動するもので、原文への新しいアクセス権は与えない。
