# nginx-sitemap

nginx の設定ファイルを静的に解析し、リクエストの経路をマップとして出力するツールです。  
どの IP:port と Host で待ち受け、どの location がどのバックエンドへ転送されるかを一覧できます。

トラフィックの計測や nginx の設定変更・reload は行わず、設定ファイルのみを入力とします。  
systemd timer による定期実行を想定しています。

- 開発者向け情報: [docs/development.md](docs/development.md)
- 設計: [docs/DESIGN.md](docs/DESIGN.md)
- nginx の挙動の根拠: [docs/spec-basis.md](docs/spec-basis.md)

## インストール

### ビルド済みバイナリを使う場合

Release からアーカイブを取得し、同梱の `install.sh` を実行します。

```bash
VER=v0.1.0
ARCH=amd64    # または arm64
BASE="https://github.com/kazuki-sk/nginx-traffic/releases/download/$VER"
F="nginx-sitemap-$VER-linux-$ARCH.tar.gz"
curl -fsSLO "$BASE/$F"
tar xzf "$F"
cd "nginx-sitemap-$VER-linux-$ARCH"
sudo ./install.sh
```

### ソースからビルドする場合

```bash
make build
sudo make install
```

`make build` には Go が必要です。  
`make install` はビルドを行わず、ビルド済みのバイナリを配置するだけです。  
詳しくは [docs/development.md](docs/development.md) を参照してください。

### install.sh が行うこと

- バイナリを `/usr/local/bin/nginx-sitemap` に配置
- unit ファイルと timer ファイルを `/etc/systemd/system/` に配置
- 専用ユーザ `nginx-sitemap` を作成
- `/etc/nginx-sitemap/sitemap.conf` が存在しない場合、サンプルから作成
- README とサンプル類を `/usr/local/share/doc/nginx-sitemap/` に配置

既存の `sitemap.conf` は上書きしません。  
また、サービスの起動や有効化は行いません。

| オプション | 説明 |
|---|---|
| `DESTDIR=<dir>` | 指定ディレクトリ配下に配置（systemd の操作は行わない） |
| `PREFIX=<dir>` | バイナリの配置先を変更（unit の `ExecStart` も合わせて書き換え） |
| `--no-systemd` | ユーザ作成と `daemon-reload` を省略 |

## 設定

設定ファイルは `/etc/nginx-sitemap/sitemap.conf` で、nginx と同じ構文で記述します。  
nginx 本体に読み込まれないよう、`/etc/nginx` 配下には置かないでください。

```nginx
settings {
    nginx_conf        /etc/nginx/nginx.conf;
    nginx_version     1.30.0;
    assume_superuser  on;
    output_dir        /var/lib/nginx-sitemap;
    skip_unchanged    on;
}
```

| 項目 | 既定値 | 説明 |
|---|---|---|
| `nginx_conf` | `/etc/nginx/nginx.conf` | 解析対象のメイン設定ファイル |
| `nginx_version` | なし | 対象 nginx のバージョン。バージョンによって異なる挙動の判定に使用 |
| `assume_superuser` | `on` | nginx が root で動作しているか。listen を省略した server のポート決定に使用 |
| `output_dir` | `/var/lib/nginx-sitemap` | 出力先ディレクトリ |
| `skip_unchanged` | `off` | 設定が前回から変更されていなければ生成を省略 |
| `resolve` | `off` | 生成時点の DNS 解決結果を併記 |
| `dot` | `on` | `map.dot` も出力 |
| `mask_header` | なし | マスク対象のヘッダを追加 |
| `mask_paths` | なし | マスクするファイルパス（glob で指定） |
| `mask_values` | `on` | `off` で秘密情報のマスクを無効化 |
| `inherited_show` | なし | location ごとに表示する継承ディレクティブを追加 |
| `inherited_hide` | なし | 上記の表示から除外するディレクティブ |
| `stale_after` | `180d` | 手動エントリの確認日がこれより古い場合に警告 |
| `read_certificates` | `off` | `ssl_certificate` の証明書を読み込み、有効期限を表示 |

`assume_superuser` は、listen を指定していない server の待ち受けポートを決めるための設定です。  
`on` の場合は `*:80`、`off` の場合は `*:8000` とみなします。  
nginx.org のパッケージは master プロセスを root で起動するため、通常は `on` のままで問題ありません。

`resolve on` を使う場合は、systemd の drop-in も必要です。  
詳しくは [unit の制限](#unit-の制限) を参照してください。

`read_certificates on` を使う場合は、証明書ファイルの読み取り権限が必要です。  
詳しくは [読み込むファイル](#読み込むファイル) を参照してください。

全項目を記載したサンプルは `/usr/local/share/doc/nginx-sitemap/sitemap.conf.example` にあります。

### nginx を経由しないサービスを追加する

nginx を経由しないサービスも、手動で記述すればマップに含められます。  
共通基盤から独立させた監視系などの記載に使えます。

```nginx
external grafana-mon {
    group     monitoring;
    endpoint  https://grafana.mon.internal:3000;
    backend   10.0.5.10:3000;
    depends   prometheus-mon;
    owner     infra-mon;
    note      "nginx 障害時も使えるよう独立させている";
    reviewed  2026-11-01;
}

annotate server api.example.internal {
    group  "API";
    owner  team-api;
    note   "外部 SaaS からの Webhook を受ける";
    link   管理画面  https://api.example.internal/admin/;
    link   Grafana   https://grafana.mon.internal/d/api;
}

include /etc/nginx-sitemap/manual.d/*.conf;
```

| ブロック | 指定できる項目 |
|---|---|
| `external <名前>` | `group` `endpoint` `listen` `backend` `depends` `owner` `note` `tag` `link` `reviewed` |
| `annotate server <server_name>` | `group` `owner` `note` `tag` `link` |
| `annotate upstream <名前>` | `owner` `note` `tag` `expect` |
| `annotate stream_server <名前>` | `group` `owner` `note` `tag` `link` |
| `annotate stream_upstream <名前>` | `owner` `note` `tag` `expect` |

手動で記述した要素には「manual」の印が付きます。  
`annotate` は注記を追加するだけで、nginx の設定から読み取った内容は変更しません。  
`expect` は想定するメンバの宣言で、nginx 側のメンバとは区別して表示されます。

`group` はサイト一覧と構成図でのまとまりです。  
指定しない場合は、ドメインごとにまとめます（`shop.example.co.jp` なら `example.co.jp`）。  
`link <名前> <URL>` は、管理画面やダッシュボード、手順書などへのリンクです。複数指定できます。

次の場合は警告を出力します。

- `annotate` の対象が nginx の設定に存在しない
- `reviewed` の日付が `stale_after` より古い
- `depends` の参照先が定義されていない

## 実行

まず手動で 1 回実行して結果を確認し、問題がなければ timer を有効化します。

```bash
sudo systemctl start nginx-sitemap.service
journalctl -u nginx-sitemap.service -n 20
sudo systemctl enable --now nginx-sitemap.timer
```

timer は 15 分ごとに起動し、起動時刻は最大 60 秒ずれます。  
停止していた間に実行されなかった分は、次回起動時に 1 回だけ実行されます。

コマンドラインから直接実行することもできます。

```bash
nginx-sitemap -nginx-conf /etc/nginx/nginx.conf -out /tmp/sitemap
```

| フラグ | 説明 |
|---|---|
| `-config <file>` | 設定ファイル（既定: `/etc/nginx-sitemap/sitemap.conf`） |
| `-nginx-conf <file>` | 解析対象のメイン設定ファイル |
| `-out <dir>` | 出力先ディレクトリ（存在しなければ作成） |
| `-nginx-version <ver>` | 対象 nginx のバージョン |
| `-skip-unchanged` | 設定が前回から変更されていなければ生成を省略 |
| `-resolve` | 生成時点の DNS 解決結果を併記 |
| `-version` | バージョンを表示 |

フラグは設定ファイルの値より優先されます。  
`-config` を省略し、既定の場所にファイルがない場合は既定値で動作します。  
`-config` で指定したファイルが存在しない場合はエラーになります。

| 終了コード | 意味 |
|---|---|
| 0 | 成功（設定に変更がなく生成を省略した場合を含む） |
| 1 | 出力は完了したが、解析エラーがある |
| 2 | 出力に失敗 |

終了コード 1 の場合、unit は failed になります。  
解析エラーを見落とさないよう、意図的にこの動作にしています。

## 出力

出力先には次の 3 ファイルが生成されます。

| ファイル | 説明 |
|---|---|
| `index.html` | ブラウザで閲覧する一覧。外部ファイルを読み込まない |
| `map.json` | 機械処理向けのデータ。スキーマは [schema/map.schema.json](schema/map.schema.json) |
| `map.dot` | Graphviz 形式のグラフ。`dot -Tsvg map.dot -o map.svg` で画像化できる |

`index.html` には次の画面があります。

| 画面 | 説明 |
|---|---|
| 構成図 | 待ち受け、サイトのグループ、upstream の全体図。サイトを選ぶとパス、upstream、宛先までの経路を表示 |
| サイト一覧 | サイトをグループごとに一覧表示。外の URL から各サイトを開ける |
| 台帳 | サイトごとの外の URL、中の URL、証明書の期限、担当、リンク。宛先からの逆引きも可能 |
| 設定 | サイトに適用される設定を、include を展開して 1 つにまとめて表示 |
| 解析の詳細 | 解析エラー、警告、読み込んだファイル、map などの分岐表 |

外の URL は利用者がアクセスする URL で、server_name と listen から作成します。  
中の URL は nginx の転送先です。nginx を経由せずにバックエンドへ直接アクセスする場合に使えます。  
証明書の残り日数は、ページを開いた時点で計算します。  
手動で追加したサービスも、サイト一覧、台帳、構成図に表示されます。

各要素には、元になった設定の `ファイル:行` が付きます。  
変数を含む値は、解決の度合いに応じて次の 4 状態に分類されます。

| 状態 | 意味 |
|---|---|
| static | 変数を含まない |
| resolved | 変数を含むが、値が 1 つに定まった |
| conditional | 条件ごとの値の表として表せた |
| dynamic | リクエストなどに依存し、解決できない |

dynamic の値には理由が付きます。  
`index.html` では conditional と dynamic を色分けして表示します。

`index.html` は単一ファイルで完結しているため、nginx が停止していても手元にコピーすれば閲覧できます。

## 社内向けに公開する

出力を nginx で配信するためのサンプル設定を同梱しています。  
ツール自体はこの設定を適用しません。

1. `/usr/local/share/doc/nginx-sitemap/nginx-sitemap.nginx.conf.sample` をコピーします。
2. IP アドレス、ホスト名、許可する CIDR、証明書のパスを環境に合わせて書き換えます。
3. `nginx -t` で確認してから nginx を reload します。

サンプルは内部 IP のみで待ち受け、許可した CIDR からのアクセスだけを受け付けます。  
配信するのは 3 つの出力ファイルのみで、それ以外へのリクエストには 404 を返します。

公開する前に、次の点を確認してください。

- マップには内部構成がすべて含まれます。アクセスを許可する範囲は運用チームに限定してください。
- IP 制限だけでは不十分な場合があります。必要に応じて認証を追加してください。
- nginx の前段に LB がある場合、IP 制限が機能しないことがあります。realip の設定を確認してください。
- 秘密情報のマスクは、一般的な記述パターンしか検出しません。初回は `map.json` の内容を確認してください。

なお、この公開用 server も nginx の設定の一部としてマップに含まれます。

## 権限とセキュリティ

### 読み込むファイル

読み込むのはメイン設定と、`include` されたファイルのみです。秘密鍵、パスワードファイル、htpasswd は開きません。

証明書ファイルは、`read_certificates on` の場合に限り読み込みます。  
証明書と秘密鍵を 1 つのファイルにまとめている場合、そのファイルは読み込みません。

Let's Encrypt の証明書は root 以外から参照できない場所にあります。  
`read_certificates on` で使う場合は、証明書ファイルとその親ディレクトリに限って ACL で権限を付与してください。

```bash
setfacl -m u:nginx-sitemap:x /etc/letsencrypt/live /etc/letsencrypt/archive
setfacl -m u:nginx-sitemap:x /etc/letsencrypt/live/example.com /etc/letsencrypt/archive/example.com
setfacl -m u:nginx-sitemap:r /etc/letsencrypt/archive/example.com/fullchain*.pem
```

`privkey*.pem` には権限を付与しないでください。

### 実行ユーザ

専用ユーザ `nginx-sitemap` で動作し、root 権限は使用しません。

このユーザには nginx の設定ファイルの読み取り権限が必要です。  
nginx.org のパッケージの既定のパーミッションであれば、追加の設定は不要です。  
読み取れない設定ファイルがある場合は、そのファイルに限って ACL で読み取り権限を付与してください。

```bash
setfacl -m u:nginx-sitemap:r /etc/nginx/conf.d/example.conf
```

秘密鍵やパスワードファイルには権限を付与しないでください。

### 出力ファイルの権限

出力先 `/var/lib/nginx-sitemap` のディレクトリは 0755、ファイルは 0644 で作成されます。  
nginx のワーカープロセスはこの権限でファイルを読み取れます。

### unit の制限

unit では次の制限を設定しています。

- 書き込み可能なのは出力先のみ
- `/etc/nginx` と `/etc/nginx-sitemap` は読み取り専用
- `/etc/ssl/private` などの秘密鍵の配置場所にはアクセス不可
- ネットワークの使用不可
- 特権の取得と危険なシステムコールを禁止
- メモリは 64MB、CPU は 50% が上限
- CPU と I/O の優先度を低く設定

秘密鍵を別の場所に置いている場合は、unit の `InaccessiblePaths=` に追加してください。

`resolve on` を使う場合に限り、ネットワークの制限を解除する必要があります。  
`/usr/local/share/doc/nginx-sitemap/resolve.conf.example` を `/etc/systemd/system/nginx-sitemap.service.d/resolve.conf` にコピーしてください。

### 秘密情報のマスク

次の値は、出力時に `***masked***` に置き換えます。

- Authorization、Proxy-Authorization、Cookie、X-Api-Key、X-Auth-Token ヘッダの値
- `secure_link_secret` と `secure_link_md5` の値
- 名前に secret、token、password などを含む `set` 変数の値
- `fastcgi_param` などで、認証情報と思われる名前のパラメータの値
- `ssl_certificate_key` に直接記述された鍵

社内独自のヘッダは `mask_header` で追加できます。  
ファイルパスをマスクする場合は `mask_paths` を使用してください。

## 既知の制限

静的解析のため、次の項目は解決できません。  
解決できない値は、理由を付けて出力します。

### リクエストや実行環境に依存する値

- Host ヘッダ、Cookie、クエリ文字列など、リクエストによって変わる値は解決できません。
- 接続元アドレスや TLS の情報は解決できません。
- ファイルの存在有無は判定できません。`try_files` の分岐は条件付きで表示します。
- `$hostname` は実行ホストに依存するため、値を出力しません。

### DNS

- 既定では名前解決の結果を出力せず、ホスト名のまま表示します。
- `resolve on` の結果は生成時点のものです。nginx が実際に使用しているアドレスと一致するとは限りません。
- 変数を含む転送先のホスト名は nginx がリクエストごとに解決するため、どのアドレスになるかは判定できません。

### 正規表現

- 先読みや後方参照など、PCRE 固有の構文は評価しません。
- 評価した正規表現でも、細部の挙動が PCRE と異なる場合があります。
- location の正規表現を評価できない場合、内部リダイレクト先は解決しません。

### 書き換えと内部リダイレクト

- `rewrite` の置換先にキャプチャを含む場合、遷移先は解決できません。
- 内部リダイレクトは 1 段ずつ表示し、連鎖やループは追跡しません。
- `if` の条件は評価可能なものだけを評価し、それ以外は条件として表示します。
- `if` ブロック内の設定と外側の設定のマージは再現しません。
- 別の location で `set` された変数の値は解決できません。
- `X-Accel-Redirect`、`proxy_intercept_errors`、`auth_request` など、upstream の応答に依存する遷移は条件付きで表示するのみです。

### モジュールとディレクティブ

- njs、perl、lua などで定義された変数は解決できません。
- サードパーティモジュールのディレクティブは、エラーにせず読み飛ばします。
- `split_clients` の割り当て結果は計算せず、割合の表として表示します。
- `geo` の `ranges` と `proxy` は評価しません。
- `proxy_pass` の URI 部分による書き換え結果は計算しません。
- `proxy_redirect` と `sub_filter` は反映しません。
- `mail` ブロックは解析しません。

### server の選択

- TLS の SNI と Host ヘッダが異なる場合の選択は表現しません。
- HTTP/2 のコネクション再利用時の選択は表現しません。
- listen にホスト名を指定した場合、複数アドレスへの展開は行いません。
- listen を省略した server のポートは、`assume_superuser` の設定によって決まります。
- `$server_name` は、server_name が 1 つだけの場合に限り解決します。

### 継承の表示

- location ごとの継承は、指定したディレクティブのみ表示します。
- `allow` と `deny` について、クライアントごとの判定は行いません。

### 構文と検証

- 構文の正しさの最終判断は `nginx -t` に委ねます。本ツールが再現するのは nginx のエラーの一部のみです。
- 構文エラーを含むファイルは、エラー箇所より前の部分のみ解析します。
- 大文字・小文字を区別しないファイルシステムは考慮していません。
- 内部リダイレクト先の URI は正規化しません。
- `http` ブロックと `stream` ブロックは、それぞれ最初の 1 つのみ解析します。

### バージョン

- nginx 1.30.0 を前提としています。
- 1.31.5 で追加された predicate location には対応していません。

### サイトと証明書の情報

- ワイルドカードや正規表現の server_name には、外の URL を作成しません。
- 中の URL は upstream のメンバから作成します。ネットワークの構成によっては、直接アクセスできない場合があります。
- グループ分けのドメイン判定は、`co.jp` などの主要な 2 階層ドメインのみに対応しています。それ以外は末尾の 2 ラベルで判定します。
- 変数を含む `ssl_certificate` や、`data:` 形式で記述された証明書は読み込みません。
- 証明書と秘密鍵を 1 つのファイルにまとめている場合、有効期限は表示されません。

### 秘密情報

- マスクの対象は、特定の名前のヘッダ、ディレクティブ、変数のみです。
- map の値や `return` の本文に記述された秘密情報はマスクされません。

## ライセンス

本ツールは [nginx-go-crossplane](https://github.com/nginxinc/nginx-go-crossplane) v0.4.89（Apache License 2.0）を使用しています。  
ライセンス文は Release の tar.gz に同梱しています。
