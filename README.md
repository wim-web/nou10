# nou10

GitHubから外向き通信で指示と配布物を取得する、Go製のデプロイエージェントです。
GitHub Deploymentsに要求と結果を記録し、同じリポジトリのRelease assetを取得して、
AppSpecに従った展開・ファイル配置・フック実行をGoコード内で行います。
CodeDeploy AgentやAWS SDKの導入は不要です。

```text
GitHub-hosted Actions → GitHub Deployments / Releases ← nou10 agent
                                                          ↓
                                                nou10内蔵の配備処理
                                                          ↓
                                                AppSpec / hooks / app
```

デプロイ用の受信ポート、SSH、self-hosted runner、AWS CodeDeployサービスへの登録は使いません。

## v0.1の範囲

- Linux、1ターゲット（repository × application × environment）につき1ホスト・1プロセス。
- AppSpecのLinux向け主要項目に対応します。詳細は [AppSpec対応範囲](docs/appspec.md) を参照してください。
- 配置先はホスト設定の `install_root` 配下に限定し、フックはagentと同じユーザーで実行します。
- tgzのみ。ルートに `appspec.yml` と `manifest.json`、実行可能な `ValidateService` フックが必要です。
- bboltの同期トランザクションで台帳・結果送信待ちを保存し、OSの `flock` で同一ホストの二重起動を防ぎます。
- `request_id` が同じ要求は1回の実行にまとめ、各Deployment IDへ状態を報告します。内容の違う再使用は拒否します。
- 実行中断・タイムアウト・実行開始後のクラッシュでは結果不明として保留します。自動再実行・自動ロールバックはしません。

CLI名と既定taskは、仕様案の仮称 `nouto` から **`nou10` / `deploy:nou10`** にしています。
taskは設定で変更できます。実装の選択と検証範囲は [設計メモ](docs/design.md) にまとめています。

## ビルドとテスト

Go 1.25以上。CGOは不要です。ホストagentはLinux向け、要求作成・状態確認CLIはLinux/macOSで利用できます。

```sh
go test -race ./...
go vet ./...
go build -trimpath -o bin/nou10 ./cmd/nou10

# 配備先のCPUに合わせる
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/nou10-linux-amd64 ./cmd/nou10
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o bin/nou10-linux-arm64 ./cmd/nou10

# Docker内のUbuntu 24.04で、非rootユーザーによる実配備テスト
sh scripts/test-linux.sh
```

LinuxテストはCodeDeployをインストールせず、nou10のGoコードで実際に配置とフックを実行します。
初回配備・更新・前回の停止フック・権限・既存ファイルの扱い・正常性検証・失敗・タイムアウトを確認します。
模擬GitHub APIから実配備までの結合試験では、結果再送時の重複実行防止と成功履歴の引き継ぎも確認します。

## nou10本体のリリース

[Release workflow](.github/workflows/release.yml) はmainへのpushで
[release-please](https://github.com/googleapis/release-please-action) を実行します。

1. `feat:`・`fix:` などのConventional CommitsからRelease PRを自動作成・更新します。
2. Release PRでバージョン・CHANGELOG・`cmd/nou10/version.go` をまとめて更新します。
3. Release PRをマージすると、release-pleaseがタグとdraft Releaseを作成します。
4. 同じworkflowで対象コミットのCI・ビルド・成果物の添付を行い、成功後にReleaseを公開します。

タグ作成・バージョン入力・別workflowの手動起動は不要です。設定は
[release-please-config.json](release-please-config.json) と [.release-please-manifest.json](.release-please-manifest.json) で管理します。
CIのrace検査・vet・Linux配備テストが通ると、CGO無効・デバッグ情報削減で次の4種類をビルドします。

- `nou10_0.1.0_linux_amd64.tar.gz`
- `nou10_0.1.0_linux_arm64.tar.gz`
- `nou10_0.1.0_darwin_amd64.tar.gz`
- `nou10_0.1.0_darwin_arm64.tar.gz`
- `checksums.txt`（上記4ファイルのSHA-256）

各アーカイブにはバイナリ、LICENSE、README、docs、examplesが入ります。`nou10 version` にrelease-pleaseが決めたバージョンを埋め込みます。
macOS版は要求作成・状態確認用で、agentの対応OSはLinuxです。

通常のリリースはLatestに設定します。検証・ビルド・アップロードが失敗した場合はdraftを残し、
Actionsの **Re-run failed jobs** で続きから再実行できます。公開済みの成果物は上書きしません。
タグのイベント連鎖に依存せず、release-pleaseの出力から同じworkflow内の後続ジョブへつなぎます。

GitHubのSettings → Actions → Generalで、ActionsによるPR作成を許可してください。
通常は標準の `GITHUB_TOKEN` を使います。Release PRの作成・更新時にもPR用CIを自動起動させたい場合は、
Contents・Pull requests・Issuesの書き込み権限を持つトークンを `RELEASE_PLEASE_TOKEN` secretに設定します。
標準トークンが作成するPRでは、新しいworkflow実行が抑制されるためです。公開前のCIはどちらの場合もこのworkflowで実行します。

GitHubへの公開を行わずに、ローカルで成果物を作ることもできます。

```sh
bash scripts/release.sh build v0.1.0
# Linux: cd dist && sha256sum -c checksums.txt
# macOS: cd dist && shasum -a 256 -c checksums.txt
```

## ホストの初期設定

Ubuntu 24.04を初期検証対象としています。実際のアプリの権限・起動方法・健康確認はホスト上で受入確認してください。

1. 専用ユーザー `nou10` と、配備先ディレクトリを作成します。
2. `nou10` バイナリを `/usr/local/bin/nou10` に配置します。
3. [examples/config.yml](examples/config.yml) を `/etc/nou10/myapp-production.yml` にコピーし、
   repository・application・environment・`install_root` を設定します。
   `install_root` は実在する絶対パスで、シンボリックリンクを含まないパスを指定します。
   `state_dir`・`lock_dir`・`token_file` と重なる場所には設定できません。
4. 対象リポジトリだけに限定したfine-grained PATを `/etc/nou10/github-token` に配置します。
   所有者は `nou10`、モードは `0600` または `0400`。必要な権限は **Contents: read / Deployments: read and write** です。
   トークンを設定YAMLやコマンド引数へ直接書かないでください。

ディレクトリの例（管理者が実施）:

```sh
sudo useradd --system --home-dir /var/lib/nou10 --shell /usr/sbin/nologin nou10
sudo install -d -o root -g nou10 -m 0750 /etc/nou10
sudo install -d -o nou10 -g nou10 -m 0700 /var/lib/nou10/myapp-production
sudo install -d -o nou10 -g nou10 -m 0755 /run/nou10
sudo install -d -o nou10 -g nou10 -m 0755 /srv/myapp
# 上記の設定ファイルとtoken_fileを配置してから実行
sudo -u nou10 /usr/local/bin/nou10 doctor --config /etc/nou10/myapp-production.yml
```

`doctor` は設定・トークンファイル・ローカルロック/台帳・配置先の書き込み・GitHubの読み取りを検査します。
Deploymentsの書き込み権限は、読み取りだけでは証明できないため結果に明記します。
稼働中のagentがロックを持っている場合は、ローカル管理コマンドの前にサービスを停止します。

### systemdで起動

[nou10@.service](examples/systemd/nou10@.service) を `/etc/systemd/system/` に配置します。

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now nou10@myapp-production.service
sudo journalctl -u nou10@myapp-production.service -f
```

**`initialized deployment baseline; ready for new requests` を確認してから、最初のDeploymentを作成します。**
初回取得時に存在した履歴は実行しません。初期化に失敗している間も実行しません。

同一ホストの全agentで `lock_dir` を共有してください。複数ターゲットの配置先は重ならないようにします。
`state_dir` はターゲットごとに固定し、
台帳を削除・置換したり、古いバックアップに戻して運転を再開したりしないでください。
同一ターゲットを別ホストで消費することは防げないため、1ホストへの割り当ては運用で維持します。

## アプリ側の準備とGHA

[examples/bundle](examples/bundle) のAppSpecとscriptsをアプリの `deploy/` にコピーして編集します。
サンプルは `/srv/myapp/myapp` をsystemdで起動し、`http://127.0.0.1:8080/health` の成功を確認します。
[myapp.service](examples/systemd/myapp.service) と [限定したsudoers例](examples/systemd/myapp.sudoers) も用意しています。
サービスは専用ユーザーで実行し、sudoersは `visudo` で確認してから導入します。curl、sudo、systemdが必要です。

配布物は次の構造にします。リンク・デバイス・setuid/setgidファイルは受け付けません。

```text
bundle.tgz
├── appspec.yml
├── manifest.json       # {"source_sha":"<完全な40文字commit SHA>"}
├── app/
└── scripts/            # フックには実行ビットが必要
```

[examples/deploy.yml](examples/deploy.yml) をアプリ側リポジトリのworkflowとして利用できます。
Goのビルド対象・CPU・default branch名・アプリ名・環境・**nou10の固定commit SHA** を先に変更してください。
テストとビルドに成功したジョブの配布物だけを、同じリポジトリのReleaseへ公開します。
Release/assetの公開に失敗した場合はDeploymentを作成しません。PATへの自動フォールバックはありません。
GHAの自動Deploymentはtaskが異なるためnou10は無視します。

### 既存assetから要求する

`GITHUB_TOKEN` はGHAの環境変数として渡します。手元のCLIでは `--token-file` も使えます。

```sh
nou10 deploy \
  --repository owner/repository --application myapp --environment production \
  --sha 0123456789abcdef0123456789abcdef01234567 \
  --release-id 123 --asset-id 456 --sha256 "$BUNDLE_SHA256" \
  --request-id manual-20261003-01 --start-within 15m \
  --wait --wait-timeout 30m

nou10 status --repository owner/repository --deployment-id 789
nou10 status --repository owner/repository --deployment-id 789 --wait
```

`deploy` はJSONで作成したDeployment IDを出力します。GHAではrequest IDをrun ID・run attempt・ターゲットから生成できます。
意図的な再実行や過去assetへの切り戻しは、**新しいrequest ID** で要求してください。
同じIDを再送する場合は `--start-before` を含めて元の内容を完全に一致させます。
作成APIの通信断では作成成否が不明な場合があるため、自動POST再試行はせずエラーを返します。

`--wait` は `success` の観測時だけ終了コード0です。`failure` / `error` / `inactive`、認証エラー、
待機上限、キャンセルは非0です。待機タイムアウトやGHAのキャンセルは配備先の処理を停止しません。
通常の `status` は観測した状態を表示し、コマンド自体の読み取り成功を終了コード0で示します。

## 中断・障害への対応

- **報告だけ失敗**: 終了結果を台帳に保存してから送信するため、次のポーリングで報告だけを再送します。
  報告待ちが解消するまでは、新規実行も待機します。API側で削除されたDeploymentなどへの報告が恒久的に失敗する場合も同様です。
- **通信断・5xx**: 最大15分までバックオフします。レート制限は `Retry-After` / reset時刻を守ります。
- **401・通常403**: 新規実行を止め、少なくとも15分待ちます。token_fileを安全に置き換えると次回API呼び出しで読み直します。
- **実行開始後のクラッシュ・シグナル・タイムアウト**: `error` と結果不明を保存し、そのターゲットを保留します。
  実行中のサービス停止も保留を発生させます。可能なら配備が完了してからagentを停止してください。

保留からの復旧:

```sh
sudo systemctl stop nou10@myapp-production.service
sudo -u nou10 nou10 inspect --config /etc/nou10/myapp-production.yml
# deploy.log、実際のアプリ状態、残ったフックのプロセスを確認し、必要なら停止する
sudo -u nou10 nou10 recover --config /etc/nou10/myapp-production.yml --confirm-stopped
sudo systemctl start nou10@myapp-production.service
# 初期化完了ログを確認し、新しいrequest IDで再配備する
```

`recover` は不明な実行の記録を残し、受理済みの未実行要求を取り消して、新しい初期化基準位置を取得させます。
既存のファイルやDBの変更を戻す処理は行いません。
停止はプロセスグループに対して試みますが、フックが別セッションへ離脱した場合などは停止を証明できません。
このため、タイムアウトは常に保留として扱います。副作用のexactly-once実行は保証しません。

## ログとデータ

- journal: エージェントの開始・保留・処理結果・通信エラー。
- `<state_dir>/jobs/<Deployment ID>/deploy.log`: 配備の進捗とフックの標準出力・標準エラー。
- `<state_dir>/jobs/<Deployment ID>/bundle.tgz`: 検証済み配布物。
- `<state_dir>/ledger.db`: 初期化基準位置、要求とDeployment IDの対応、実行状態・時刻、未送信結果、前回成功の参照。
- `<state_dir>/jobs/<Deployment ID>/archive/`: 展開済み配布物。次回配備で前回成功版の停止フックを使います。
- `<state_dir>/jobs/<Deployment ID>/installation.json`: その版が管理する配置済みファイルの一覧。

v0.1は台帳・配布物を自動削除しません。ディスク使用量を監視し、前回成功したジョブの配布物・展開先・配置履歴や
台帳の参照を壊す一括削除を避けてください。API一覧は全ページを条件付き取得するため、長期運用での履歴増大にも注意が必要です。

## 信頼境界

workflow、Releaseを書き換えられる主体、AppSpecとフックを信頼します。
SHA-256とsource SHAは指定された配布物との一致を確認するもので、発行者の認証にはなりません。
外部PRのコードをデプロイ権限付きジョブで実行しないでください。

フックへ渡す環境変数は固定のPATHとLANGから構成し、PATやagentの環境を引き継ぎません。
ただしフックをagentと同じユーザーやrootで実行すると、資格情報ファイル・ホストへアクセスできる権限があります。
環境別の強い資格情報分離、任意コードのサンドボックス、rootからのPAT隔離は提供しません。
PATのDeployments: writeは要求作成にも使えます。

設定・AppSpecの配備先・権限設計・PATの更新は運用者が管理します。フックは配布物内の実行可能ファイルを引数配列で起動し、
payloadからURL・シェルコマンド・実行ユーザー・配備先を指定することはできません。

## ライセンス

nou10本体は [MIT License](LICENSE) で提供します。

## 参考

- [GitHub Deployments API](https://docs.github.com/en/rest/deployments/deployments)
- [GitHub Deployment statuses API](https://docs.github.com/en/rest/deployments/statuses)
- [GitHub Release assets API](https://docs.github.com/en/rest/releases/assets)
- [GitHub REST API best practices](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api)
- [AppSpecファイル形式](https://docs.aws.amazon.com/codedeploy/latest/userguide/reference-appspec-file-structure.html)
