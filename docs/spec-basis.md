# nginx の挙動の根拠

ツールが前提にしている nginx の挙動と、その根拠をまとめる。
対象は nginx 1.30.0。

根拠には次の 2 種類がある。

- doc: nginx の公式ドキュメント
- src: nginx 1.30.0 のソースコード

ドキュメントに書かれていない点は、ソースコードを根拠にした。
ソースは https://nginx.org/download/nginx-1.30.0.tar.gz から取得した。
コードの該当箇所にも、同じ根拠をコメントで書いている。

## 設定ファイルの読み込み

### include の相対パス

相対パスは、メイン設定のあるディレクトリを基準に解決する。
include を書いたファイルの場所は関係しない。

- src: `src/core/ngx_conf_file.c` の `ngx_conf_include`。`ngx_conf_full_name(cycle, &file, 1)` で conf prefix を基準にする。

### include の glob

ファイル名はバイト順に並べる。
`*` などは、先頭が `.` のファイルにはマッチしない。
glob が何にもマッチしなくてもエラーにしない。
glob を使わずに書いたファイルが無い場合はエラーにする。

- src: `src/os/unix/ngx_files.c` の `ngx_open_glob`。glob(3) をフラグなしで呼ぶ。
- src: nginx は `setlocale()` を呼ばないので、並び順はバイト順になる。

### include の循環

nginx は循環を検出しない。
1.30.0 の `nginx -t` は、循環があると Segmentation fault で落ちる。
ツールは循環をエラーとして記録し、残りを解析する。

- Docker の nginx:1.30.0 で確認した。

### http と stream のブロック

それぞれ 1 つしか書けない。

- src: `ngx_http_block` と `ngx_stream_block` が "is duplicate" を返す。

## listen

### listen を省略した場合

superuser で動いていれば `*:80`、そうでなければ `*:8000` になる。

- doc: `ngx_http_core_module` の listen
- src: `ngx_http_core_server` の `(getuid() == 0) ? 80 : 8000`

### アドレスだけを書いた場合

ポートは 80 になる。

- doc: `ngx_http_core_module` の listen

### 受け付けるパラメータ

http の listen が受け付けるのは次のものだけ。

`default_server` `default` `ssl` `http2` `quic` `proxy_protocol` `bind` `deferred` `reuseport` `multipath` `setfib=` `fastopen=` `backlog=` `rcvbuf=` `sndbuf=` `accept_filter=` `so_keepalive=` `ipv6only=`

stream では `default`、`http2`、`quic` が無く、`udp` がある。

- src: `ngx_http_core_listen` と `ngx_stream_core_listen`

### default_server

`default_server` を書いた server が、その address:port の既定になる。
書かれていなければ、その address:port を最初に使った server が既定になる。
1 つの address:port に `default_server` が 2 つあるとエラーになる。

- doc: `ngx_http_core_module` の listen
- src: `src/http/ngx_http.c` の `ngx_http_add_addresses` と `ngx_http_add_address`

### フラグのまとめ方

同じ address:port の server の間で、`ssl` `http2` `quic` `proxy_protocol` は OR でまとまる。
`backlog` などのソケットのオプションは、1 つの address:port に 1 回しか書けない。

- src: `ngx_http_add_addresses`

### ワイルドカードと個別アドレスの共存

`*:80` と `10.0.0.1:80` が両方ある場合、address:port ごとに server の一覧と既定を持つ。
`10.0.0.1:80` に来た接続は、`10.0.0.1:80` の server の中から選ぶ。
`*:80` の server は候補にならない。

`bind` を付けていなければ、ソケットは `*:80` だけが開く。
接続先のアドレスは `getsockname()` で調べて振り分ける。

- src: `ngx_http_init_listening` と `ngx_http_add_addrs`

### stream の listen

stream の server には listen が必須。

- src: `ngx_stream_core_server` の "no "listen" is defined for server"

## server_name

### 優先順位

次の順に探し、最初にマッチしたものを使う。

1. 完全一致
2. 先頭がワイルドカードの名前のうち、最も長いもの
3. 末尾がワイルドカードの名前のうち、最も長いもの
4. 正規表現。設定に書いた順

- doc: https://nginx.org/en/docs/http/server_names.html

### 名前の扱い

`.example.com` は、`example.com` と `*.example.com` の両方にマッチする。
名前は小文字にしてから比べる。
server_name を書かない server の名前は `""` になる。

- doc: server_names.html
- doc: `ngx_http_core_module` の server_name
- src: `ngx_http_core_server_name` の `ngx_strlow`

### 名前の重複

同じ address:port で同じ名前が 2 回出てきたら、2 つ目以降は無視する。

- src: `ngx_http_server_names` の "conflicting server name ... ignored"

### stream の server_name

stream で server_name を使えるのは 1.25.5 から。

- doc: `ngx_stream_core_module` の server_name

## location

### 探す順番

1. `=` の location が URI と一致すれば、それを使う。
2. prefix の location のうち、最も長く一致するものを覚える。
3. 覚えた location の中にネストした location があれば、同じ手順で探す。
4. 覚えた location が `^~` でなければ、正規表現の location を書いた順に試す。
5. どの正規表現にもマッチしなければ、覚えた prefix の location を使う。

- doc: `ngx_http_core_module` の location
- src: `ngx_http_core_find_location` と `ngx_http_core_find_static_location`

### 末尾が / の location

prefix が `/` で終わり、`proxy_pass` などで処理する location がある。
その場合、末尾の `/` が無い URI には 301 を返す。

- doc: `ngx_http_core_module` の location
- src: `*_pass` のディレクティブが `auto_redirect` を立てる。

### 修飾子の書き方

`location =/foo` のように、修飾子と URI を続けて書いてもよい。

- src: `ngx_http_core_location`

### predicate location

`location $変数` は 1.31.5 で追加された。
1.30.0 では、`$` で始まる普通の prefix として扱う。

- doc: `ngx_http_core_module` の location

## rewrite モジュール

### 実行の順番

`set`、`if`、`rewrite`、`return`、`break` は、書いた順に実行する。
server レベルのものは、location を探す前に実行する。
location レベルのものは、location が決まった後に実行する。

- doc: https://nginx.org/en/docs/http/ngx_http_rewrite_module.html
- src: `ngx_http_rewrite_handler`

### ネストした location への継承

継承しない。
ネストした location では、その location に書いたものだけを実行する。

- src: `ngx_http_rewrite_merge_loc_conf`。子に何も無ければ、親のものを引き継がない。

### if の中身

`if` の中身は、外側と同じ命令の並びの中で実行する。
条件が成り立つと、`if` の中の設定が使われる。
`if` の中に `proxy_pass` があれば、その `proxy_pass` が使われる。

- src: `ngx_http_rewrite_if` の `nlcf->codes = lcf->codes`
- src: `ngx_http_script_if_code` が `r->loc_conf` を切り替える。

### if の条件

変数だけの条件は、値が空か `0` なら偽になる。
`=` と `!=` は文字列の比較。
`~` と `~*` は正規表現。`!` を付けると否定になる。
`-f` `-d` `-e` `-x` はファイルの検査。

- doc: `ngx_http_rewrite_module` の if

### return の書き方

`return URL;` は 302 を返す。

- doc: `ngx_http_rewrite_module` の return

### 変数の寿命

変数の値は、内部リダイレクトの後も残る。

- src: `ngx_http_internal_redirect` は `r->variables` を消さない。

## 変数

### 変数の書き方

`$` の後に数字 1 文字なら、正規表現のキャプチャ。
`${名前}` は括弧付きの変数名。
それ以外は、英数字とアンダースコアの並びが変数名。

- src: `src/http/ngx_http_script.c` の `ngx_http_script_compile`

### server から決まる変数

`$server_port`、`$server_addr`、`$scheme`、`$https` は、server の listen から決める。
`$document_root` は root か alias の値。
`$server_name` は、server_name が 1 つだけのときに決める。

- doc: `ngx_http_core_module` の Embedded Variables

### 変数の循環

変数が自分自身を参照すると、nginx は実行時にエラーを出す。
ツールは循環として記録する。

- src: `ngx_http_get_indexed_variable` の "cycle while evaluating variable"

## map、geo、split_clients

### map の照合

値を小文字にしてから比べる。
次の順に探す。

1. 完全一致
2. 先頭がワイルドカードのキー。`hostnames` を指定した場合だけ
3. 末尾がワイルドカードのキー。`hostnames` を指定した場合だけ
4. 正規表現。値が空でない場合だけ。書いた順
5. `default`

- doc: https://nginx.org/en/docs/http/ngx_http_map_module.html
- src: `src/http/ngx_http_variables.c` の `ngx_http_map_find`

### geo

最も長い CIDR が優先される。

- doc: https://nginx.org/en/docs/http/ngx_http_geo_module.html
- src: `ngx_http_geo_cidr_value` と `ngx_radix32tree_find`

## upstream

### 静的な proxy_pass の宛先

同じ名前の upstream があれば、それを使う。
名前の大文字と小文字は区別しない。
upstream を定義した位置は、proxy_pass の前でも後でもよい。
upstream 名にポートを付けるとエラーになる。

同じ名前の upstream が無ければ、`ホスト:ポート` の暗黙の upstream を作る。
ホスト名は、設定を読み込むときに 1 回だけ解決する。

- src: `src/http/ngx_http_upstream.c` の `ngx_http_upstream_add`

### 変数を使った proxy_pass の宛先

リクエストのたびに、まず upstream の中から探す。
名前に加えて、ポートも一致する必要がある。
見つからなければ resolver で名前を解決する。

- doc: `ngx_http_proxy_module` の proxy_pass
- src: `ngx_http_upstream_init_request`

### server の既定値

`weight=1`、`max_fails=1`、`fail_timeout=10s`、`max_conns=0`。
`resolve` を付けると、実行中も名前を解決し直す。

- doc: `ngx_http_upstream_module` の server

## nginx 自身への転送

### 受け直す listen

`proxy_pass` の宛先が、nginx 自身の listen に一致する場合に、自分自身への転送とみなす。
宛先と同じ address:port の listen があれば、その listen で受ける。
無ければ、同じポートのワイルドカードの listen で受ける。
ワイルドカードで受けるとみなすのは、ループバックアドレスと、ほかの listen に書かれた IP アドレスだけ。

- src: `ngx_http_init_listening` と `ngx_http_add_addrs`（「ワイルドカードと個別アドレスの共存」と同じ）

### 受け直す server

nginx が転送時に送る Host ヘッダで、通常のリクエストと同じ順に server を選ぶ。
`proxy_set_header Host` が無い場合の Host は `$proxy_host`。
`$proxy_host` は、`proxy_pass` に書いたホスト名とポート。

- doc: `ngx_http_proxy_module` の proxy_set_header と、Embedded Variables の `$proxy_host`

## 継承

### 配列のディレクティブ

`proxy_set_header`、`add_header`、`error_page`、`allow` と `deny` が該当する。
今のレベルに 1 つも書かれていない場合だけ、上のレベルから継承する。

- doc: `ngx_http_proxy_module` の proxy_set_header
- doc: `ngx_http_headers_module` の add_header
- src: `ngx_http_core_merge_loc_conf` の `error_pages`

### proxy_set_header の既定値

何も書かなければ、`Host $proxy_host` と `Connection close` だけを設定する。

- doc: `ngx_http_proxy_module` の proxy_set_header

### 継承しないもの

`proxy_pass` などの `*_pass`、`alias`、`try_files` は継承しない。

- doc: `ngx_http_core_module` の alias
- src: try_files のモジュールは、location の設定をマージしない。
- src: `*_pass` は、その location のハンドラを設定する。

### root

`root` は継承する。

- doc: `ngx_http_core_module` の root
