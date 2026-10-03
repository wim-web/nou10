# nou10のAppSpec対応範囲

nou10は `appspec.yml` 形式を使い、配備処理をGoで実装しています。
CodeDeploy Agentのインストールは不要です。CodeDeploy全体の完全互換は提供しません。

## 対応する設定

| 設定 | 動作 |
| --- | --- |
| `version` | `0.0` のみ |
| `os` | `linux` のみ |
| `files[].source` | 配布物内の通常ファイルまたはディレクトリ。`/` は配布物全体。`..`・リンクは禁止 |
| `files[].destination` | ホスト設定の `install_root` 配下の絶対ディレクトリ。ファイルなら元のファイル名、ディレクトリならその内容を配置 |
| `file_exists_behavior` | `DISALLOW`（既定）、`OVERWRITE`、`RETAIN` |
| `permissions[].object` | `install_root` 内の絶対パス |
| `permissions[].owner` / `group` | ホスト上で解決できるユーザー・グループ名。OSの権限で許可された変更のみ可能 |
| `permissions[].mode` | `0000`〜`0777` の8進表記。`"0644"` のように文字列で指定することを推奨 |
| `permissions[].type` | `file` / `directory` のリスト。下記の適用範囲を参照 |
| `hooks` | `ApplicationStop`、`BeforeInstall`、`AfterInstall`、`ApplicationStart`、`ValidateService` |
| 各フックの `location` | 配布物内の実行可能な通常ファイル。各イベントに複数指定でき、記述順に実行 |
| 各フックの `timeout` | 秒数。1〜3600、省略時3600。同一イベント内の合計も3600以下。ホスト設定の全体上限も適用 |
| 各フックの `runas` | 省略可能。指定する場合はagentの実行ユーザーと同じUIDのユーザー名のみ |

権限指定は `type` を省略するとobject自身と配下全体に適用します。
`type` を指定する場合、objectはディレクトリに限定します。
`file` は直下の通常ファイル、`directory` は配下のディレクトリに適用し、object自身は含みません。
リンク・特殊ファイル・ハードリンクされた通常ファイルに対する権限変更は拒否します。

`permissions` の `pattern`・`except`・`acls`・SELinux `context`、別ユーザーへの切り替え、
ロードバランサー用フック、`Install` など予約イベントへのスクリプト指定、その他の不明な項目は拒否します。
未対応項目を無視して配備することはありません。

## 実行順序

1. 配布物とAppSpec、フック、配置先、ファイル競合を検査する。
2. **前回成功した版**の `ApplicationStop` を実行する。初回は省略する。
3. 今回の版の `BeforeInstall` を実行する。
4. 配置計画を再確認し、不要になった管理対象ファイルの削除、ファイル配置、権限変更を行う。
5. 今回の版の `AfterInstall` → `ApplicationStart` → `ValidateService` を実行する。
6. 配置履歴を保存し、正常性確認まで成功した場合だけ成功とする。

`ValidateService` は1つ以上必須です。フックの作業ディレクトリは、そのフックが入っている展開済み配布物のルートです。
配布物はフック開始前にダウンロード・検証・展開します。CodeDeployの通常サービスとダウンロードの順序は一致しません。
`BeforeInstall` で作成しないと存在しないsourceは事前検査で拒否されます。

## 既存ファイルと更新

前回成功した配備が管理しているファイルは更新できます。それ以外の通常ファイルとの競合は次のとおりです。

- `DISALLOW`: 競合として失敗する。最初の検査で分かる競合はフック開始前に拒否する。
- `OVERWRITE`: 今回の内容で置き換え、nou10の管理対象にする。
- `RETAIN`: 既存ファイルをそのまま残す。nou10の管理対象には取り込まず、後続配備の掃除でも削除しない。

前回の管理対象で、今回の配置に含まれないファイルは削除します。空ディレクトリは残します。
同じパスのファイルとディレクトリを入れ替える配置、配置先のシンボリックリンクや特殊ファイル、
複数のsourceが同じファイルへ出力する配置は拒否します。新しく作る配置先ディレクトリのモードは0755です。
ファイルは配布物の通常のpermission bitsを引き継ぎ、tarの所有者情報は適用しません。

ファイル単位では一時ファイルとrenameで置き換えますが、配備全体のロールバックは行いません。
失敗した配備で初めて作ったファイルは前回成功の一覧に含まれないため、次回のDISALLOWで競合する場合があります。
運用者が状態を確認して削除するか、意図した新規要求でOVERWRITEを指定してください。

## フックの環境と実行権限

フックには固定のPATH・LANGと、次の値だけを渡します。

- `APPLICATION_NAME`: ホスト設定のapplication
- `DEPLOYMENT_ID`: 今回実行する代表Deployment ID
- `DEPLOYMENT_GROUP_ID`: repository・application・environmentから計算したターゲットID
- `DEPLOYMENT_GROUP_NAME`: `<application>-<environment>`
- `LIFECYCLE_EVENT`: 実行中のイベント名
- `NOU10_SOURCE_SHA`: 今回要求されたコミットSHA

PATHは `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`、LANGは `C.UTF-8` です。
PATやagent起動時の環境変数は渡しません。AppSpec中の文字列をシェルコマンドに連結することもありません。
シェルスクリプトはshebangで指定されたシェルを通して通常の実行ファイルとして起動します。

フックはagentと同じユーザーの権限で動きます。同一ユーザーの資格情報ファイルを読み取れないようにする隔離機能はありません。
別ユーザーとしてのサービス起動が必要なら、承認済みのスクリプトでsystemdや限定したsudoersルールを利用します。

## 以前のCodeDeploy呼び出し版からの移行

ホスト設定から `codedeploy:` を削除し、`install_root:` を設定してください。
以前のCodeDeploy版の配置履歴は自動変換しません。成功履歴に新形式の `installation.json` がなければ配備を拒否します。
既に配備したホストを移行する場合は、アプリとフックの停止・既存ファイルを運用者が確認し、
履歴を保全したうえで新しいstate_dirを初期化し、必要に応じてOVERWRITEによる新規要求で管理対象を取り込みます。

形式の参考: [AppSpec構造](https://docs.aws.amazon.com/codedeploy/latest/userguide/reference-appspec-file-structure.html)、
[files](https://docs.aws.amazon.com/codedeploy/latest/userguide/reference-appspec-file-structure-files.html)、
[permissions](https://docs.aws.amazon.com/codedeploy/latest/userguide/reference-appspec-file-structure-permissions.html)、
[hooks](https://docs.aws.amazon.com/codedeploy/latest/userguide/reference-appspec-file-structure-hooks.html)。
