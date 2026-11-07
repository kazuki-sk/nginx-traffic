# nginx-sitemap

nginx の設定ファイルを静的に解析し、リクエストの流れをマップにするツール。
どの IP:port と Host で待ち受け、どの location がどのバックエンドに流れるかを出力する。

トラフィックは計測しない。
nginx の設定は変更せず、reload もしない。
systemd timer から定期的に起動し、生成したら終了する。

- 開発者向けの情報: [docs/development.md](docs/development.md)
- 設計: [docs/DESIGN.md](docs/DESIGN.md)
- nginx の挙動の根拠: [docs/spec-basis.md](docs/spec-basis.md)

## インストール

### Release から入れる

GitHub Release の tar.gz を使う。
配置先のサーバーに Go や make は要らない。

```bash
VER=v0.1.0
ARCH=amd64    # または arm64
gh release download "$VER" --repo kazuki-sk/nginx-traffic -p "nginx-sitemap-$VER-linux-$ARCH.tar.gz*"
sha256sum -c "nginx-sitemap-$VER-linux-$ARCH.tar.gz.sha256"
tar xzf "nginx-sitemap-$VER-linux-$ARCH.tar.gz"
cd "nginx-sitemap-$VER-linux-$ARCH"
sudo ./install.sh
```

リポジトリは private なので、`curl` では取得できない。
サーバーから GitHub に出られない場合は、手元で取得した tar.gz を持ち込む。

### ソースから入れる

```bash
make build
sudo make install
```

`make build` には Go が必要。
`make install` はビルドしない。ビルド済みのバイナリを配置するだけ。
詳しくは [docs/development.md](docs/development.md) を参照。

### install.sh がすること

- バイナリを `/usr/local/bin/nginx-sitemap` に置く。
- unit と timer を `/etc/systemd/system/` に置く。
- ユーザ `nginx-sitemap` を作る。
- `/etc/nginx-sitemap/sitemap.conf` が無ければ、サンプルから作る。
- README とサンプル類を `/usr/local/share/doc/nginx-sitemap/` に置く。

既にある `sitemap.conf` は上書きしない。
サービスの起動と有効化はしない。

| オプション | 内容 |
|---|---|
| `DESTDIR=<dir>` | 指定したディレクトリの下に配置する。systemd は操作しない |
| `PREFIX=<dir>` | バイナリの置き場所を変える。unit の `ExecStart` も合わせて書き換える |
| `--no-systemd` | ユーザ作成と `daemon-reload` をしない |

## 設定

設定ファイルは `/etc/nginx-sitemap/sitemap.conf`。
書き方は nginx の設定と同じ。
nginx が読み込まないように、`/etc/nginx` の外に置く。

```nginx
settings {
    nginx_conf        /etc/nginx/nginx.conf;
    nginx_version     1.30.0;
    assume_superuser  on;
    output_dir        /var/lib/nginx-sitemap;
    skip_unchanged    on;
}
```

| 項目 | 既定値 | 内容 |
|---|---|---|
| `nginx_conf` | `/etc/nginx/nginx.conf` | 解析するメイン設定 |
| `nginx_version` | なし | 対象の nginx のバージョン。バージョンで変わる挙動の判定に使う |
| `assume_superuser` | `on` | nginx が root で動いているか。listen を省略した server のポートが決まる |
| `output_dir` | `/var/lib/nginx-sitemap` | 出力先 |
| `skip_unchanged` | `off` | 設定が前回と同じなら生成しない |
| `resolve` | `off` | 生成時点の DNS の解決結果を併記する |
| `dot` | `on` | `map.dot` も出力する |
| `mask_header` | なし | 値を伏せるヘッダを追加する |
| `mask_paths` | なし | 伏せるファイルパスを glob で指定する |
| `mask_values` | `on` | `off` にすると秘密情報を伏せなくなる |
| `inherited_show` | なし | location ごとに表示する継承ディレクティブを追加する |
| `inherited_hide` | なし | 上の表示から外すディレクティブ |
| `stale_after` | `180d` | 手動エントリの確認日がこれより古いと警告する |

`assume_superuser` は、listen を書いていない server の待ち受けポートを決める。
`on` なら `*:80`、`off` なら `*:8000` とみなす。
nginx.org のパッケージは master プロセスを root で起動するので、通常は `on` でよい。

`resolve on` を使う場合は、systemd の drop-in も必要になる。
詳しくは [unit の制限](#unit-の制限) を参照。

全項目のサンプルは `/usr/local/share/doc/nginx-sitemap/sitemap.conf.example` にある。

### nginx を通らないものを書き足す

nginx を経由しないサービスも、手で書けばマップに載る。
共通基盤から独立させた監視系などに使う。

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
    owner  team-api;
    note   "外部 SaaS からの Webhook を受ける";
}

include /etc/nginx-sitemap/manual.d/*.conf;
```

| ブロック | 書ける項目 |
|---|---|
| `external <名前>` | `group` `endpoint` `listen` `backend` `depends` `owner` `note` `tag` `reviewed` |
| `annotate server <server_name>` | `owner` `note` `tag` |
| `annotate upstream <名前>` | `owner` `note` `tag` `expect` |
| `annotate stream_server <名前>` | `owner` `note` `tag` |
| `annotate stream_upstream <名前>` | `owner` `note` `tag` `expect` |

手で書いた要素には「manual」の印が付く。
`annotate` は注記を足すだけで、nginx から読み取った内容は変えない。
`expect` は想定するメンバの宣言で、nginx 側のメンバとは別に表示する。

次の場合は警告を出す。

- `annotate` の対象が nginx の設定に無い
- `reviewed` の日付が `stale_after` より古い
- `depends` の相手が定義されていない

## 動かす

```bash
sudo systemctl start nginx-sitemap.service
journalctl -u nginx-sitemap.service -n 20
sudo systemctl enable --now nginx-sitemap.timer
```

timer は 15 分ごとに起動する。
起動時刻は最大 60 秒ずれる。
止まっていた間の分は、次の起動時に 1 回だけ実行する。

手動でも実行できる。

```bash
nginx-sitemap -nginx-conf /etc/nginx/nginx.conf -out /tmp/sitemap
```

| フラグ | 内容 |
|---|---|
| `-config <file>` | 設定ファイル。既定は `/etc/nginx-sitemap/sitemap.conf` |
| `-nginx-conf <file>` | 解析するメイン設定 |
| `-out <dir>` | 出力先。無ければ作る |
| `-nginx-version <ver>` | 対象の nginx のバージョン |
| `-skip-unchanged` | 設定が前回と同じなら生成しない |
| `-resolve` | 生成時点の DNS の解決結果を併記する |
| `-version` | バージョンを表示する |

フラグは設定ファイルより優先する。
`-config` を省略し、既定の場所にファイルが無い場合は、既定値で動く。
`-config` で指定したファイルが無い場合はエラーになる。

| 終了コード | 意味 |
|---|---|
| 0 | 成功。設定が同じで生成を省いた場合も含む |
| 1 | 出力はしたが、解析エラーがある |
| 2 | 出力できなかった |

終了コード 1 では unit が failed になる。
エラーに気づけるように、あえてそうしている。

## 出力

出力先には 3 つのファイルができる。

| ファイル | 内容 |
|---|---|
| `index.html` | ブラウザで見る一覧。外部のファイルを読み込まない |
| `map.json` | 機械向けのデータ。スキーマは [schema/map.schema.json](schema/map.schema.json) |
| `map.dot` | Graphviz のグラフ。`dot -Tsvg map.dot -o map.svg` で画像になる |

`index.html` では次のものを見られる。

- listen ごとの server の一覧と、server_name の優先順位
- location の木と、それぞれの転送先
- upstream のメンバ
- map などの分岐表
- 手動で書き足したサービス
- 解析エラーと警告

各要素には、元になった設定の `ファイル:行` が付く。
変数を含む値は、どこまで決まったかで次の 4 つに分かれる。

| 状態 | 意味 |
|---|---|
| static | 変数を含まない |
| resolved | 変数を含むが、値が 1 つに決まった |
| conditional | 条件ごとの値の表になった |
| dynamic | リクエストなどに依存し、決まらない |

dynamic の値には理由が付く。
`index.html` では conditional と dynamic を色で示す。

`index.html` は 1 ファイルで完結している。
nginx が止まっていても、手元にコピーすれば開ける。

## 社内に公開する

出力を nginx で配信するためのサンプルがある。
ツールはこの設定を適用しない。

1. `/usr/local/share/doc/nginx-sitemap/nginx-sitemap.nginx.conf.sample` をコピーする。
2. IP、ホスト名、許可する CIDR、証明書のパスを書き換える。
3. `nginx -t` で確かめてから、nginx を reload する。

サンプルは内部の IP だけで待ち受け、許可した CIDR からだけ見られる。
配信するのは 3 つの出力ファイルだけで、それ以外は 404 を返す。

公開する前に、次の点を確認する。

- マップには内部の構成がすべて載る。許可する範囲は運用チームに絞る。
- 必要なら認証を足す。IP の制限だけでは足りないことがある。
- nginx の前に LB がある場合、IP の制限が効かないことがある。realip の設定を確認する。
- 秘密情報を伏せる処理は、よくある書き方しか検出しない。初回は `map.json` の中身を確認する。

この公開用の server も、nginx の設定の一部としてマップに載る。

## 権限とセキュリティ

### 読むもの・読まないもの

読むのは、メイン設定と `include` されたファイルだけ。
鍵、パスワードのファイル、htpasswd、証明書は開かない。

### 実行ユーザ

専用ユーザ `nginx-sitemap` で動く。
root は使わない。

このユーザには、設定ファイルを読む権限が要る。
nginx.org のパッケージの既定の権限なら、追加の設定は要らない。
読めない設定ファイルがある場合は、そのファイルだけ ACL で読めるようにする。

```bash
setfacl -m u:nginx-sitemap:r /etc/nginx/conf.d/example.conf
```

鍵やパスワードのファイルには、権限を与えない。

### 出力の権限

出力先は `/var/lib/nginx-sitemap` で、ディレクトリは 0755 になる。
ファイルは 0644 で書く。
nginx のワーカーは、この権限でファイルを読める。

### unit の制限

unit では次の制限をかけている。

- 書き込めるのは出力先だけ
- `/etc/nginx` と `/etc/nginx-sitemap` は読み取り専用
- `/etc/ssl/private` などの鍵の置き場所には触れない
- ネットワークを使わない
- 特権の取得と、危険なシステムコールを禁止
- メモリは 64MB、CPU は 50% まで
- CPU と I/O の優先度を下げる

鍵を別の場所に置いている場合は、unit の `InaccessiblePaths=` に追加する。

`resolve on` を使う場合だけ、ネットワークの制限を外す。
`/usr/local/share/doc/nginx-sitemap/resolve.conf.example` を `/etc/systemd/system/nginx-sitemap.service.d/resolve.conf` にコピーする。

### 秘密情報を伏せる

次の値は、出力で `***masked***` に置き換える。

- Authorization、Proxy-Authorization、Cookie、X-Api-Key、X-Auth-Token ヘッダの値
- `secure_link_secret` と `secure_link_md5` の値
- 名前に secret、token、password などを含む `set` 変数の値
- `fastcgi_param` などの、認証情報らしい名前のパラメータの値
- `ssl_certificate_key` に直接書かれた鍵

社内独自のヘッダは `mask_header` で追加する。
ファイルパスを伏せたい場合は `mask_paths` を使う。

## 既知の制限

静的な解析なので、次のものは決めきれない。
決まらない値には、理由を付けて出力する。

### リクエストや環境で決まる値

- Host ヘッダ、Cookie、クエリなど、リクエストで変わる値は決まらない。
- 接続元のアドレスや TLS の情報は決まらない。
- ファイルがあるかどうかは分からない。`try_files` の分岐は条件付きで示す。
- `$hostname` は実行するホストで決まるので、値を出さない。

### DNS

- 名前の解決結果は既定では出さない。名前のまま示す。
- `resolve on` の結果は、生成した時点のもの。nginx が実際に使っているアドレスと一致するとは限らない。
- 変数を使った転送先の名前は、nginx がリクエストごとに解決する。どのアドレスになるかは分からない。

### 正規表現

- PCRE だけの書き方は評価しない。先読みや後方参照などが該当する。
- 評価した正規表現の細かい挙動は、PCRE と違うことがある。
- location の正規表現を評価できない場合、内部リダイレクト先を決めない。

### 書き換えと内部リダイレクト

- `rewrite` の置き換え先にキャプチャを含む場合、遷移先は決まらない。
- 内部リダイレクトは 1 段ずつ示す。連鎖やループはたどらない。
- `if` の条件は評価できるものだけ評価する。それ以外は条件として示す。
- `if` の中の設定が外側とどう合わさるかは再現しない。
- 別の location で `set` した変数の値は決まらない。
- upstream の応答で決まる遷移は、条件付きで示すだけ。`X-Accel-Redirect`、`proxy_intercept_errors`、`auth_request` などが該当する。

### モジュールとディレクティブ

- njs、perl、lua などの変数は決まらない。
- サードパーティのディレクティブは読み飛ばす。エラーにはしない。
- `split_clients` の割り当て結果は計算しない。割合の表で示す。
- `geo` の `ranges` と `proxy` は評価しない。
- `proxy_pass` の URI 部分による書き換え結果は計算しない。
- `proxy_redirect` と `sub_filter` は反映しない。
- `mail` ブロックは解析しない。

### server の選択

- TLS の SNI と Host ヘッダが違う場合の選択は表さない。
- HTTP/2 で接続を使い回す場合の選択は表さない。
- listen にホスト名を書いた場合、複数のアドレスに分けない。
- listen を省略した server のポートは `assume_superuser` の設定で決まる。
- `$server_name` は、server_name が 1 つだけのときに決める。

### 継承の表示

- location ごとの継承は、選んだディレクティブだけ表示する。
- `allow` と `deny` は、クライアントごとの判定をしない。

### 構文と検証

- 構文の正しさの最終判断は `nginx -t` に任せる。ツールは nginx のエラーの一部しか再現しない。
- 構文エラーのあるファイルは、エラーの位置より前だけを解析する。
- 大文字と小文字を区別しないファイルシステムは考慮しない。
- 内部リダイレクト先の URI は正規化しない。
- `http` と `stream` のブロックは、それぞれ最初の 1 つだけ解析する。

### バージョン

- nginx 1.30.0 を前提にしている。
- 1.31.5 で追加された predicate location には対応しない。

### 秘密情報

- 伏せるのは、決まった名前のヘッダ、ディレクティブ、変数だけ。
- map の値や `return` の本文に書いた秘密は伏せない。

## ライセンス

[nginx-go-crossplane](https://github.com/nginxinc/nginx-go-crossplane) v0.4.89 を使っている。
ライセンスは Apache License 2.0。
ライセンス文は Release の tar.gz に同梱している。
