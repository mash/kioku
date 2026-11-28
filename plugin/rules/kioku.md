# Kiokuの自動コンテクスト

Kiokuに対応づけたプロジェクトの作業では、最初に `kioku` スキルを読む。Claude Code の hook は trigger 語が1つの project に一致すると対象を選び、手元の Wiki の入口を渡して背景で pull する。一覧が出た場合は、その指示に従って session select する。背景の pull 中は手元の Wiki が最新とは限らない。入口から必要な本文を読む。

原資料の送信と保存禁止の扱いは [kioku スキル](../skills/kioku/SKILL.md) に従う。複数段階の作業では、保存済みの進捗と根拠を読む。

プロジェクトの判定と共有許可はスキルの selection 手順に従う。URL や引用も project 選択の手がかりに使う。対象が一意に決まらなければアカウントの General を使う。保存禁止とチームの共有範囲を守る。背景の pull と capture は hook が起動するもので、原文への新しいアクセス権は与えない。
