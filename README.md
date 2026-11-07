# nginx-sitemap

nginx の設定ファイルを**静的に解析して**、「どの IP:port / Host で待ち受け、どの location が、どのバックエンドに流れるか」のマップを作るツール。

- トラフィックの実測はしない（アクセスログ解析・プローブ・nginx モジュール追加なし）。
- nginx の設定を一切変更しない。reload / restart もしない。
- Go 製。CGO なしの単一静的バイナリ。systemd timer から起動され、終了する（常駐しない）。
- 出力: `map.json`（機械可読。スキーマは [`schema/map.schema.json`](schema/map.schema.json)）、`index.html`（外部リソースなしの単一ファイル）、`map.dot`（Graphviz）。

設計の経緯は [docs/DESIGN.md](docs/DESIGN.md) を参照。

---

## 1. ビルド

```bash
make build        # CGO_ENABLED=0 go build -trimpath → bin/nginx-sitemap（静的リンク）
make test         # go test ./...
make verify-fixtures   # Docker の nginx:1.30.0 で fixture に nginx -t を流す
```

Go 1.23 以上。実行時の依存は標準ライブラリと [nginx-go-crossplane](https://github.com/nginxinc/nginx-go-crossplane)（Apache-2.0、v0.4.89）だけ。

## 2. 使い方

```bash
nginx-sitemap [-config /etc/nginx-sitemap/sitemap.conf] [-nginx-conf /etc/nginx/nginx.conf]
              [-out /var/lib/nginx-sitemap] [-resolve] [-skip-unchanged] [-nginx-version 1.30.0]
```

- コマンドラインのフラグは、設定ファイル（`sitemap.conf`）の値より優先される。
- `-config` を省略した場合、既定のパスにファイルが無ければ既定値で動く。`-config` を明示した場合、ファイルが無いと致命的エラーになる。

| 終了コード | 意味 |
|---|---|
| 0 | 成功（ハッシュが同じで再生成をスキップした場合も含む） |
| 1 | 部分成功。出力は書かれ、エラーの一覧が `map.json` / `index.html` に含まれる |
| 2 | 致命的失敗（メイン設定が読めない、出力先に書けない、など） |

書き込みは「同じディレクトリに一時ファイル → fsync → chmod 0644 → rename → ディレクトリを fsync」の順で行う。`map.json` は最後に置き換えるので、途中で落ちても「完成済み」を示すハッシュが部分的な出力の横に残ることはない。

## 3. 配置

```bash
sudo make install                       # バイナリ、unit、sysusers、sitemap.conf（既にあれば上書きしない）
sudo systemd-sysusers                   # ユーザ nginx-sitemap を作成
sudoedit /etc/nginx-sitemap/sitemap.conf
sudo systemctl daemon-reload
sudo systemctl start nginx-sitemap.service   # 1 回だけ試す
journalctl -u nginx-sitemap.service -n 20
sudo systemctl enable --now nginx-sitemap.timer
```

| ファイル | 置き場所 |
|---|---|
| `deploy/systemd/nginx-sitemap.service` | `/etc/systemd/system/`（Type=oneshot） |
| `deploy/systemd/nginx-sitemap.timer` | `/etc/systemd/system/`（`OnCalendar=*:0/15`、`Persistent=true`、`RandomizedDelaySec=60`） |
| `deploy/systemd/nginx-sitemap.sysusers.conf` | `/usr/lib/sysusers.d/nginx-sitemap.conf` |
| `deploy/systemd/nginx-sitemap.service.d/resolve.conf.example` | `resolve on` のときだけ `/etc/systemd/system/nginx-sitemap.service.d/resolve.conf` に置く |
| `deploy/sitemap.conf.example` | `/etc/nginx-sitemap/sitemap.conf` |
| `deploy/nginx/nginx-sitemap.conf` | 公開用 server ブロックのサンプル（**自動適用しない**。§6） |

### 3.1 実行ユーザと最小権限

- **専用の固定ユーザ `nginx-sitemap` を使う（root は使わない）。**
  - DynamicUser を使わないのは、`StateDirectory=` と組み合わせると実体が `/var/lib/private/`（root のみ 0700）の下に置かれ、nginx ワーカーが出力を読めなくなるため。
- **読み取りに必要な権限:** メイン設定と、`include` で読まれる全ファイルの r、それらを含むディレクトリの x。
  - nginx.org のパッケージ既定（`/etc/nginx` 配下は root:root の 0644 / 0755）なら追加設定は要らない。
  - 一部の設定ファイルが 0640 などの場合は、そのファイルにだけ ACL を付ける（例 `setfacl -m u:nginx-sitemap:r /etc/nginx/conf.d/x.conf`）。nginx グループへの追加より範囲を絞れる。
- **鍵やパスワードのファイルには権限を与えない。** ツールは `include` 対象以外のファイルを一切開かない。開かないファイルの例: `ssl_certificate_key`、`ssl_password_file`、`auth_basic_user_file`、証明書。unit 側でも `InaccessiblePaths=-/etc/ssl/private -/etc/nginx/tls/private` で読めなくしている（鍵の置き場所に合わせて追加する）。
- **出力先:** `StateDirectory=nginx-sitemap` で `/var/lib/nginx-sitemap` を作る（所有者 nginx-sitemap、0755）。`UMask=0022` でファイルは 0644 になり、nginx ワーカー（nginx.org パッケージでは `nginx`）から読める。

### 3.2 ハードニング（unit に設定済み）

- **ファイルシステム:** `ProtectSystem=strict` と `ReadWritePaths=/var/lib/nginx-sitemap` で、書き込めるのは出力先だけ。ほかに `ReadOnlyPaths=/etc/nginx /etc/nginx-sitemap`、`ProtectHome`、`PrivateTmp`、`PrivateDevices`。
- **ネットワーク:** `PrivateNetwork=yes` と `RestrictAddressFamilies=AF_UNIX`。`resolve on` のときだけ drop-in で解除する。
- **権限と syscall:**
  - `NoNewPrivileges`、`CapabilityBoundingSet=`（空）、`MemoryDenyWriteExecute`
  - `SystemCallFilter=@system-service ~@privileged`（拒否した呼び出しは `SystemCallErrorNumber=EPERM` で失敗させる）
  - `ProtectKernel*`、`ProtectProc=invisible` など
- **リソース:** `MemoryMax=64M`、`CPUQuota=50%`、`Nice=10`、`IOSchedulingClass=idle`、`TimeoutStartSec=60`
- **部分成功の扱い:** 部分成功（exit 1）では unit が failed になる。意図的にそうしていて、エラーに気づけるようにするため。不要なら `SuccessExitStatus=1` のコメントを外す。

> 確認できた範囲: このリポジトリでは `systemd-analyze verify` を通し、`systemd-run --user` で次のオプションを付けた実行を確認した。
> - syscall フィルタ、`MemoryDenyWriteExecute`、`PrivateNetwork`、`MemoryMax=64M`、`RestrictNamespaces`
>
> 次の項目は root が必要なため実機では未検証:
> - `User=`、`StateDirectory=`、`ProtectSystem=strict`、`InaccessiblePaths=`
>
> 初回は `systemctl start` で手動実行し、journal を確認すること。

## 4. 設定ファイル（sitemap.conf）

nginx と同じ構文で書き、同じパーサで読む。そのため手動エントリにも `file:line` の出典が付く。nginx が誤って include しないよう、**`/etc/nginx` の外**に置く。全項目は [`deploy/sitemap.conf.example`](deploy/sitemap.conf.example) を参照。

| 設定 | 既定 | 説明 |
|---|---|---|
| `nginx_conf` | `/etc/nginx/nginx.conf` | メイン設定 |
| `nginx_version` | なし | `$nginx_version` の値と、バージョン依存の判定に使う（例: stream の server_name は 1.25.5 以上） |
| `assume_superuser` | `on` | listen 省略時の補完値。`on` なら `*:80`、`off` なら `*:8000` |
| `output_dir` | `/var/lib/nginx-sitemap` | 出力先 |
| `skip_unchanged` | `off` | 設定ハッシュが前回の `map.json` と同じなら何もしない（`resolve on` の実行はスキップしない） |
| `resolve` | `off` | 生成時点の DNS スナップショットを併記する |
| `dot` | `on` | `map.dot` も出力する |
| `mask_header` / `mask_paths` / `mask_values` | — | マスクの追加と無効化（§7.3） |
| `inherited_show` / `inherited_hide` | — | location ごとに表示する継承ディレクティブの追加と除外 |
| `stale_after` | `180d` | `external` の `reviewed` がこれより古ければ警告する |

### 4.1 nginx 設定に無いもの（手動エントリ）

共通基盤から独立させている監視系など、この nginx を経由しないものを、ユーザが自分で書く。

```nginx
external grafana-mon {
    group     monitoring;
    endpoint  https://grafana.mon.internal:3000;
    listen    10.0.5.10:3000 ssl;
    backend   10.0.5.10:3000;
    depends   prometheus-mon;        # 手動エントリ同士の依存
    owner     infra-mon;
    note      "共通基盤から独立。nginx 障害時の監視用";
    reviewed  2026-11-01;
}
annotate server api.example.internal { owner team-api; note "Webhook 受け"; }
annotate upstream app_backend { expect 10.0.1.11:8080; }
include /etc/nginx-sitemap/manual.d/*.conf;
```

- **出自を区別する:** 手動の要素はすべて `origin: "manual"`、`status: "declared"` になり、出典は sitemap.conf の行を指す。HTML では紫のバッジで区別する。
- **nginx 由来の内容は変えない:** `annotate` は注記を足すだけ。`expect` も「宣言された想定値」として別枠に表示する。
- **古くなった記述を検出する:**
  - 注記の対象が nginx 設定に無い → `stale-annotation`
  - `reviewed` が `stale_after` より古い → `stale-manual-entry`
  - `depends` 先が未定義 → `manual-unknown-depends`
  - いずれも警告として出力する。
- **ハッシュに含める:** 手動ファイルも設定ハッシュに含めるので、追記すれば再生成される。
- **nginx 障害時の閲覧:** マップは同じ nginx で配信するので、その nginx が落ちると Web からは見られない。`index.html` は 1 ファイルで完結しており、`file://` でも開ける。障害時に備えて手元へコピーしておける。

```bash
scp host:/var/lib/nginx-sitemap/index.html ./
```

## 5. 出力

### 5.1 map.json

- 構造は listen → server → location → handler → backend / upstream の順にたどれる。重複を避けるため、listen は server を **id で参照**し、handler は upstream を**名前で参照**する。
- すべての要素が次の 3 つを持つ:
  - `src`（`"file:line"`）
  - `origin`（`nginx` | `manual`）
  - `status`。値は次のいずれか:
    - `static`: 変数を含まない
    - `resolved`: 変数を含むが、すべて定数に畳み込めた
    - `conditional`: 有限の分岐表になり、葉はすべて定数
    - `dynamic`: 生成時点では決まらない
    - `declared`: 手動で書いた値
- `static` 以外には `reasons[]`（理由コード）が付く。理由コードの一覧:
  - `request-dependent` `connection-dependent` `filesystem-dependent` `host-dependent`
  - `runtime-dns` `unsupported-directive` `unsupported-regex` `unsupported-module`
  - `cycle` `branch-limit` `unknown-variable` `condition-dependent` `version-dependent`
  - `parse-error` `masked` `implicit-default`
- include の経路は `files[].included_from` にファイル単位で一度だけ記録する。
- 変数を含む値は `Expr` で表す: `lit` / `sym` / `concat` / `switch`（分岐表。map の連鎖は元の条件、例えば `$host` まで展開する）。
- スキーマ（JSON Schema draft 2020-12）は Go の型から生成している。`go test ./internal/model` で常に同期していることを検証し、全ゴールデンをこのスキーマで検証している。

### 5.2 index.html

- **一覧と検索:** listen 単位の一覧（default_server と server_name の選択順位表を含む）、server 一覧、upstream、map / geo / split_clients、手動エントリ、エラーと読み込んだファイルの一覧。server_name・location・宛先での検索と「dynamic / conditional のみ」の絞り込みができ、該当箇所はハイライトされる。
- **表示内容:** すべての要素の出典（include 経路はツールチップで表示）、生成時刻、設定ハッシュ。
- **外部リソース:** 一切読み込まない。CSP は `default-src 'none'` とインライン script / style のハッシュだけ。JS が無効な環境では `map.json` を見るよう案内する。

### 5.3 map.dot

Mermaid ではなく **Graphviz（DOT）** を採用した。Mermaid は既定で辺が 500 本まで、テキストが 50k 文字までという制限があり、server が数百ある規模では描画できないため。

```bash
dot -Tsvg map.dot -o map.svg
```

## 6. 公開用 nginx 設定（サンプル）

[`deploy/nginx/nginx-sitemap.conf`](deploy/nginx/nginx-sitemap.conf) は**サンプルとして提供するだけ**で、ツールは適用しない。

- **内容:** 内部 IP だけで listen し、`allow` / `deny` で公開範囲を絞る。キャッシュを無効にするヘッダ（`Cache-Control: no-store`、`etag off`）を付け、`index.html` / `map.json` / `map.dot` だけを配信してそれ以外は 404 にする。
- **適用手順:** 次のプレースホルダを置き換えてから `nginx -t` → reload する（reload は運用者が行う）。
  - IP（10.0.0.10）
  - ホスト名（nginx-map.lab.internal）
  - CIDR
  - 証明書のパス
- **マップへの反映:** この server ブロックは nginx 設定の一部なので、マップにも普通の server として現れる。fixture `publish_sample` で確認済み。

## 7. 仕様の根拠

nginx の挙動に関する判断は、**公式ドキュメント（doc）を第一の根拠とし、ドキュメントに記載がない点は nginx 1.30.0 のソース（src）を根拠にした**。この方針は合意済み。ソースは https://nginx.org/download/nginx-1.30.0.tar.gz から取得した。コード中の該当箇所にもコメントで同じ根拠を書いている。

### 7.1 主な判断

| 項目 | 判断 | 根拠 |
|---|---|---|
| include の相対パス | conf prefix（`-c` で渡したファイルのディレクトリ）を基準にする。include した側のファイルの場所ではない | src `core/ngx_conf_file.c` ngx_conf_include → `ngx_conf_full_name(cycle, &file, 1)` |
| include の glob | glob(3) を flags=0 で呼ぶ: バイト順にソートし、先頭の `.` にはマッチしない。0 件は許す。glob でない場合は読めないとエラー | src `os/unix/ngx_files.c` ngx_open_glob、`ngx_conf_include` |
| include の循環 | nginx には循環の検出がない（1.30.0 の `nginx -t` は SIGSEGV で落ちることを Docker で確認した）。ツールは `cycle` エラーとして記録し、残りを解析する | 実機の挙動 + src ngx_conf_include |
| listen を省略した場合 | superuser なら `*:80`、そうでなければ `*:8000`。`assume_superuser` で指定し、補完値として印を付ける | doc ngx_http_core_module#listen、src `ngx_http_core_server`（`getuid() == 0 ? 80 : 8000`） |
| アドレスだけの listen | ポート 80 | doc #listen |
| default_server | 明示が優先。無ければその address:port で最初の server。明示が 2 つあればエラー | doc #listen、src `ngx_http_add_addresses` / `ngx_http_add_address` |
| `*:80` と `10.0.0.1:80` の共存 | address:port ごとに server 一覧と default を別々に持つ。10.0.0.1:80 宛ての接続では、10.0.0.1:80 の server だけが候補になる。`bind` が無ければ、ソケットは `*:80` で受けて getsockname() で振り分ける | src `ngx_http_init_listening` / `ngx_http_add_addrs` |
| listen のフラグ | ssl / http2 / quic / proxy_protocol は同じ address:port の server 間で OR される。ソケットオプションは 1 回しか書けない | src `ngx_http_add_addresses` |
| listen のパラメータ | 1.30.0 で受け付けるもの以外はエラー | src `ngx_http_core_listen` / `ngx_stream_core_listen` |
| server_name の優先順位 | 完全一致 → 最長の前方ワイルドカード → 最長の後方ワイルドカード → 正規表現（出現順）。`.example.com` は example.com と `*.example.com` の両方にマッチする。名前は小文字化する | doc http/server_names.html、src `ngx_http_core_server_name`（ngx_strlow） |
| server_name の重複 | 同じ address:port 上の 2 つ目以降は無視（警告） | src `ngx_http_server_names`（"conflicting server name"） |
| server_name の既定 | `""` | doc #server_name |
| location の探索 | `=` → 最長の prefix（ネストした location を再帰的に探索）→ その prefix が `^~` でなければ同じ階層の regex を出現順に → 何もマッチしなければ記憶した prefix。末尾 `/` の prefix で `*_pass` を使う location は、URI+`/` に 301 を返す | doc #location、src `ngx_http_core_find_location` / `ngx_http_core_find_static_location`（auto_redirect） |
| predicate location（`location $var`） | 1.31.5 で追加された機能。1.30.0 では普通の prefix 文字列として扱う | doc #location（"(1.31.5)"） |
| rewrite 系ディレクティブの実行モデル | server レベルは location 探索の前、location レベルは location の rewrite フェーズで、書いた順に実行する | doc ngx_http_rewrite_module |
| rewrite 系のネスト継承 | ネストした location には**継承しない** | src `ngx_http_rewrite_merge_loc_conf`（子の codes が NULL のままなら handler は何もしない） |
| `if` の本体 | 外側と同じコード列の中で実行される。条件が真なら if 内の location 設定（proxy_pass など）が使われる | src `ngx_http_rewrite_if`（`nlcf->codes = lcf->codes`）、`ngx_http_script_if_code` |
| 変数の寿命 | 内部リダイレクトを越えて値が残る。そのため別の経路で set された値は `condition-dependent` にする | src `ngx_http_internal_redirect`（r->variables をクリアしない） |
| map の照合 | 小文字化した値で 完全一致 → (hostnames のとき) 前方ワイルドカード → 後方ワイルドカード → 正規表現（値が空でないとき、出現順）→ default | doc ngx_http_map_module、src `ngx_http_map_find` |
| geo | 最長の CIDR を優先する | src radix tree（ngx_http_geo） |
| map / 変数の循環 | nginx は実行時に "cycle while evaluating variable" を出す。ツールは `cycle` と記録する | src `ngx_http_get_indexed_variable` |
| upstream 名の解決（静的） | 同名の upstream（大小文字を区別しない、定義順は問わない）を使う。ポートを付けるとエラー。該当が無ければ host:port の暗黙 upstream になり、名前は起動時に一度だけ解決される | src `ngx_http_upstream_add` |
| upstream 名の解決（変数） | 実行時に upstream 群を探す（ポートも一致が必要）→ 無ければ resolver。resolver が無ければ警告する | doc #proxy_pass（"searched among the described server groups, and, if not found, is determined using a resolver"）、src `ngx_http_upstream_init_request` |
| upstream のメンバの既定値 | weight=1、max_fails=1、fail_timeout=10s、max_conns=0。`resolve` は実行時に再解決される（OSS では 1.27.3 以上） | doc ngx_http_upstream_module#server |
| 継承（配列型） | proxy_set_header / add_header / error_page / allow・deny は「現在のレベルに 1 つも無いときだけ」継承する。proxy_set_header の既定値は `Host $proxy_host` と `Connection close` | doc #proxy_set_header、#add_header、src `ngx_http_core_merge_loc_conf`（error_pages） |
| 継承しないもの | `*_pass`（その location のハンドラ）、alias、try_files | src: try_files モジュールは merge_loc_conf を持たない。`*_pass` は clcf->handler を設定する |
| `$server_name` | 明示された server_name がちょうど 1 つのときだけ確定させる（保守的な扱い。合意済み） | doc（"name of the server which accepted a request"） |
| `$server_port` / `$server_addr` / `$scheme` / `$https` | server の listen から確定させる。複数あれば listen ごとの分岐、ワイルドカードの `$server_addr` は不明 | doc Embedded Variables |
| stream | listen は必須。server_name は 1.25.5 以上（ssl_preread または SSL のフェーズで選択される） | doc ngx_stream_core_module、src `ngx_stream_core_server` |

### 7.2 変数の評価（実測なしの部分評価）

- **set:** server → location の順に、書いた順の命令列として抽象実行する。値が確定しない `if` は、合流点で `if` 条件の分岐表にする。`return` / `rewrite … last|break|redirect` で実行を止める。条件が静的に決まる `if`（`$a = "prod"` など）は評価する。
- **map / geo / split_clients:**
  - 入力が定数なら値を 1 つに決める。
  - 入力が未知なら分岐表に展開する。入力が別の map の結果（連鎖）なら、分岐ごとに次の map を適用し、元の条件のまま表現する。
  - split_clients は割合の表のまま出す（MurmurHash の評価はしない）。
- **組み込み変数:** §7.1 の表のとおり。それ以外は、リクエスト依存・接続依存・ホスト依存・未知の変数に分類して、シンボルとして残す。
- **正規表現:** Go の RE2 でコンパイルできる式だけを評価する。PCRE 固有の構文（先読み、後方参照など）は `unsupported-regex` にする。
- **爆発の防止:** 式のノード数が 256 を超えたら `branch-limit`（dynamic）にする。`*_pass` の宛先の列挙は 64 件まで。
- **DNS:** 既定では名前のまま出す。`resolve on` のときだけ、生成時点の解決結果を「スナップショット」として併記する（nginx 自身の解決結果ではない）。純 Go のリゾルバなので、使うのは `/etc/hosts` と `/etc/resolv.conf` だけで、nsswitch（LDAP / mDNS など）は使わない。

### 7.3 秘密情報

- **開かないファイル:** `include` の対象以外は一切開かない。鍵、パスワードファイル、htpasswd、証明書の中身は読まない。
- **既定でマスクする値:**
  - Authorization / Proxy-Authorization / Cookie / X-Api-Key / X-Auth-Token ヘッダの値（`*_set_header`、`add_header`）
  - `secure_link_secret` / `secure_link_md5` の引数
  - 名前が secret / token / password / api_key / private_key / credential に該当する `set` 変数の値
  - `*_param` の HTTP_AUTHORIZATION / PASSWORD / SECRET / TOKEN 系
  - `ssl_certificate_key data:…` / `engine:…`
- **パスのマスク:** `mask_paths`（glob）に該当するディレクティブ引数のパスと、ファイル一覧のパスをマスクする。
- **マスクの追加と無効化:** `mask_header` でヘッダを追加でき、`mask_values off` でマスクを無効にできる（非推奨）。

## 8. 性能（実測）

条件: WSL2 / x86-64、`-trimpath -ldflags "-s -w"`、`/usr/bin/time -v` で測定。

| 規模 | 時間 | 最大 RSS | map.json |
|---|---|---|---|
| server 300 / location 2,100 / include 301 ファイル | 0.31 秒 | 27 MB | 約 1.9 MB |
| server 800 / location 5,600 / include 801 ファイル | 1.3〜2.2 秒 | 43 MB | 約 5.2 MB |
| 同上、ハッシュ不変でスキップ | 0.21 秒 | 38 MB | — |

- ヒープ上限として `debug.SetMemoryLimit(40MiB)` を入れている。
- 出力は一時ファイルへ直接ストリーミングで書き、メモリ上にまとめて保持しない。

## 9. テスト

- **ゴールデン比較:** `testdata/fixtures/*/golden.map.json` と比較する（`go test ./internal/app`、更新は `-update`）。fixture は**実機の `nginx -t`（Docker の nginx:1.30.0）を通るものだけ**にしている。
  - 項目: listen 省略・default_server の暗黙決定、server_name の優先順位、location の優先順位、upstream の展開、map の連鎖、set の定数伝播、変数を使った proxy_pass、stream ブロック、include の glob、公開用サンプル、手動エントリ
- **無効な設定（別枠）:** `testdata/invalid/`。循環 include と構文エラーで、`nginx -t` が失敗することも確認する。
- **単体テスト:** location 探索、map の照合規則、if の静的評価、upstream のポートエラー、resolver なしの警告、branch-limit、マスク、include の基準パス、glob、循環、ハッシュ、HTML の CSP と外部リソースが無いこと、スキップ、原子的書き込み、DNS スナップショット、規模（600 server）。
- **fixture の検証:** `scripts/verify-fixtures.sh`。リポジトリを読み取り専用でマウントし、`--network none` で動かす。テスト用の証明書はコンテナ内で作る。`net.ipv4.ip_nonlocal_bind=1` はコンテナの netns の中だけで有効にする（サンプルのプレースホルダ IP に bind するため）。

## 10. 既知の制限（静的に解決できないケース）

ツールは**静的解析**であり、`nginx -t` の代わりにはならない（構文の妥当性の最終判断は nginx に任せる）。次のケースは、確定させずに理由コードを付けて出力するか、表現を単純化している。

**リクエスト・接続・環境に依存する値**

1. `$host` `$http_*` `$cookie_*` `$arg_*` `$uri` `$request_uri` `$args` などのリクエスト依存値、正規表現のキャプチャ（`$1` や名前付き）は、シンボルとして残す。
2. `$remote_addr`、ワイルドカード listen の `$server_addr`、`$ssl_*`、`$proxy_protocol_*`、realip は接続依存。
3. `try_files` のファイル有無、`if (-f/-d/-e/-x …)`、`$realpath_root`、symlink、index ファイルの有無は、ファイルシステムに依存する。try_files のフォールバックは条件付きの辺として出す。
4. `$hostname` と `server_name $hostname` はホストに依存する。
5. DNS について:
   - 静的な名前は nginx の起動時に一度だけ解決される。いつ解決されたかは分からない。
   - 変数を使った `*_pass` は、リクエストごとに resolver で解決される。
   - `server … resolve` は定期的に再解決される。
   - listen にホスト名を書いた場合は、起動時に解決された全アドレスで待ち受ける。
   - `resolve on` のスナップショットは、これらの結果と一致しないことがある。

**正規表現**

6. PCRE 固有の構文（先読み・後読み、後方参照、所有量指定子、`\K`、再帰など）は評価しない。
7. RE2 と PCRE で意味が異なる細部（Unicode のクラスなど）は RE2 の結果になる。
8. location の正規表現を変換できない場合、内部リダイレクト先の特定は `unsupported-regex` で保留する。

**書き換えと内部リダイレクト**

9. `rewrite` の置換にキャプチャがあると、遷移先は dynamic になる。
10. 内部リダイレクトは 1 段ずつ辺としてリンクするだけで、連鎖（最大 10 回）の追跡や、ループの検出はしない。
11. フラグなしの `rewrite` で location の rewrite フェーズが終わった後の再探索は、「rewrite」辺として近似している。
12. `if` の条件は不透明な条件として扱う。分岐内の set は分岐表に、if 内の `*_pass` は `if_blocks` として別に出す。if 内で別の設定ディレクティブを継承する細部（いわゆる "if is evil"）は再現しない。
13. upstream の応答に依存する遷移は「条件付き」としてだけ表す。例: `X-Accel-Redirect`、`proxy_intercept_errors` と `error_page`、`auth_request` のサブリクエスト、`mirror`。
14. 別の経路で `set` された変数は、内部リダイレクトを越えて値が残りうるため `condition-dependent` にする。

**モジュール**

15. njs（`js_set` / `js_content`）、perl、lua、サードパーティのモジュールが定義する変数は `unknown-variable` や `unsupported-module` になる。未知のディレクティブは黙って受け付ける（crossplane の表は 2026-05 時点のもの）。
16. split_clients のハッシュ割り当て、geo の `ranges` / `proxy` / `delete`、map の `volatile` は評価しない（表として出す）。
17. `proxy_pass` に URI 部分があるときの、location の prefix の置き換え結果は計算しない（書かれたとおりに出す）。`proxy_redirect` と `sub_filter` も反映しない。
18. `mail {}` ブロックは解析しない。stream の `pass` ディレクティブは宛先の式を出すだけで、リンクはしない。

**server と listen の選択**

19. TLS の SNI と Host ヘッダが食い違う場合や、HTTP/2 のコネクション再利用による server 選択は表現しない。stream の server_name が ssl_preread や SSL を前提にしていることも検証しない。
20. listen をホスト名で書いた場合、複数アドレスへの展開はしない。reuseport や backlog などのソケットオプションは、記録するだけで意味は解析しない。
21. listen 省略時のポートは `assume_superuser` という仮定に依存する。
22. `$server_name` は、明示された server_name が 1 つのときだけ確定させる。

**継承と検証**

23. 継承の表示は、選んだディレクティブの一覧だけ（`inherited_show` / `inherited_hide` で変更できる）。それ以外のディレクティブの継承は表示しない。allow / deny はクライアントごとの評価をしない。
24. nginx が出すエラーのうち、listen の重複・default_server の重複・upstream のポート・location のネスト・server_name の書式などは再現している。全部ではない。ディレクティブの引数の数や文脈の検査は crossplane で無効にしている。

**パーサとプラットフォーム**

25. 構文エラーのあるファイルは、エラー位置より前だけを復元する（以降は無視し、エラーとして記録する）。
26. 引用符で囲まれた文字列のエスケープの扱いは、crossplane の字句解析に従う。
27. ファイル名の大文字・小文字を区別しない OS（`ngx_filename_cmp`）は考慮しない（Linux を前提にする）。内部リダイレクト先の URI に `merge_slashes` などの正規化はしない。
28. http / stream ブロックは最初のものだけを解析する（nginx も重複はエラーにする）。

**バージョン**

29. predicate location（1.31.5 以上）には未対応（1.30.0 を前提にしている）。
30. 新しい listen パラメータは「invalid parameter」と判定する。

**マスク**

31. マスクはヒューリスティックで、既知のヘッダ・ディレクティブ・変数名に限る。map の値、`return` の本文、任意のヘッダに書いた秘密は検出しない。必要なら `mask_header` で追加する。

## 11. サードパーティ

- `github.com/nginxinc/nginx-go-crossplane` v0.4.89 — Apache License 2.0（© F5, Inc.）。バイナリを配布するときはライセンス文を同梱すること。
