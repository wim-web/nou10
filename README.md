# nou10

GitHub を介して Linux ホストへ配備する、Go 製のデプロイエージェントです。
外向き通信で GitHub Deployments の要求と Release の配布物を取得し、AppSpec に従って配備します。
SSH や受信ポートを用意せずに、GitHub-hosted Actions から配備したい場合を対象にしています。

## 利用開始

ソースからビルドする場合:

```sh
go build -trimpath -o bin/nou10 ./cmd/nou10
./bin/nou10 --help
```

配備先の agent は Linux 用です。macOS では要求作成・状態確認の CLI を利用できます。
運用前に [設計と運用上の前提](docs/design.md) を確認してください。

1. 配備先に専用ユーザー `nou10` を用意し、Linux 用バイナリを `/usr/local/bin/nou10` に置きます。
2. [ホスト設定例](examples/config.yml) を `/etc/nou10/myapp-production.yml` に置き、対象アプリに合わせて編集します。
   `install_root`・`state_dir`・`lock_dir` のディレクトリを `nou10` 所有で作成し、
   `state_dir` は `0700`、ほかの2つは `0755` にします。設定ファイルも `nou10` が読めるようにします。
3. 対象リポジトリに限定した fine-grained PAT を設定の `token_file` に置き、所有者を `nou10`、モードを `0600` にします。
   必要な権限は [Contents: read](https://docs.github.com/en/rest/releases/assets#get-a-release-asset) と
   [Deployments: read and write](https://docs.github.com/en/rest/deployments/statuses#create-a-deployment-status) です。
4. `nou10 doctor --config /etc/nou10/myapp-production.yml` を `nou10` ユーザーで実行してから、
   [systemd unit](examples/systemd/nou10@.service) を `/etc/systemd/system/` に配置し、
   `systemctl daemon-reload` と `systemctl enable --now nou10@myapp-production.service` を管理者として実行します。

初回は `initialized deployment baseline; ready for new requests` のログを確認してから要求を作成します。
初期化前から存在する要求は配備の対象になりません。

アプリ側では [AppSpec とフック](examples/bundle) を `deploy/` に、
[配備 workflow](examples/deploy.yml) を `.github/workflows/deploy.yml` にコピーして調整します。
サービスの設定は [アプリ用 unit](examples/systemd/myapp.service) と
[sudoers の例](examples/systemd/myapp.sudoers) を参照してください。

個別コマンドの使い方は `nou10 <command> --help` で確認できます。

## ライセンス

nou10本体は [MIT License](LICENSE) で提供します。
