# 設計と運用上の前提

## ホストの割り当てと状態の保全

同じターゲット（repository × application × environment）は、運用者が1ホストに割り当てます。
ローカルのロックでは別ホストの実行を排除できないためです。同一ホストの agent は `lock_dir` を共有し、
ターゲット間で配置先を重ねないでください。

台帳と前回成功した配布物・配置履歴は、次の配備の判断に必要です。
`state_dir` はターゲットごとに固定し、台帳の削除・置換・古いバックアップへの巻き戻しや、
前回成功したジョブの一括削除で運転を再開しないでください。
保管領域の監視と容量の確保は運用者が担います。

## ソースと実行処理の信頼

workflow、リポジトリや Release を変更できる主体、配備スクリプト、AppSpec とフックを信頼する運用を前提にします。
SHA-256 と source SHA の一致は配布物の照合であり、発行者の認証にはなりません。
外部 PR のコードを配備権限付きジョブで実行しないでください。

配備スクリプトとフックは agent と同じユーザーの権限を持ち、資格情報ファイルにもアクセスできます。
環境変数からトークンを除いても、ホスト上の任意コードを隔離する仕組みにはなりません。
実行ユーザー、サービス管理権限、PAT の更新は運用者が管理します。

ホストでビルドする運用では、スクリプトを渡すためだけの Release 配布物を省けるように Git を選べます。
要求から実行までにブランチが進んでも結果が指すコミットと実物がずれないよう、指定 SHA を取得します。
チェックアウトは専用にし、追跡対象ファイルをホストで編集しないでください。`.env` などホスト固有の設定は追跡対象に含めません。
Git の submodule と LFS の展開は対象外です。スクリプト自身がアプリの正常性を確認してから成功を返す必要があります。

既存の Release 配布物から Git へ切り替える場合も台帳を保持します。以前の配布物や配置ファイルを Git の処理で削除しません。
Git へ切り替えた後は、以前の AppSpec の配置履歴では現在のファイル所有関係を判断できないため、同じ台帳での配布物方式への復帰は拒否します。

## 結果不明時に保留する理由

実行開始の台帳への保存と、ホスト上の副作用を原子的には確定できません。
このため、開始意図を保存した後のクラッシュでは、実際にはフックが起動していなくても保留します。
タイムアウト時も、フックが別セッションへ離脱していれば停止を証明できません。

自動再実行は副作用を重複させる可能性があるため、運用者が実際のアプリ状態と残存プロセスを確認してから復旧します。
確認時は agent を停止し、`inspect` とジョブの `deploy.log` を参照してください。
`recover --confirm-stopped` はその確認を前提とし、ファイルや DB の変更を元に戻す操作ではありません。
アプリ固有の復旧や切り戻しは運用側で判断します。

## 台帳と結果報告

台帳には、CGO に依存せず同期トランザクションを使える bbolt を採用しています。
GitHub の結果受理とローカルの送信済み記録は原子的にできないため、結果報告の重複は許容します。
報告失敗を理由にフックを再実行しないことを優先しています。

## nou10 本体のリリースと GitHub の制約

標準の `GITHUB_TOKEN` によるタグ作成では後続の push workflow が起動しないため、
検証・ビルド・公開を同じ release workflow 内でつないでいます。
Release PR の CI を承認操作なしで起動させたい場合は、`RELEASE_PLEASE_TOKEN` に別のトークンを設定します。
標準トークンが作成・更新する PR の CI は承認待ちになる場合があります。
詳しくは [GitHub のイベント発火条件](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow#triggering-a-workflow-from-a-workflow) を参照してください。

標準トークンで Release PR を作成するには、
[Actions による PR 作成を許可するリポジトリ設定](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/enabling-features-for-your-repository/managing-github-actions-settings-for-a-repository#preventing-github-actions-from-creating-or-approving-pull-requests) も必要です。
