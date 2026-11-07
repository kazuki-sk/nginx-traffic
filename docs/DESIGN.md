# nginx-sitemap 設計

ステータス: **v1.0 / 2026-11-07 実装済み**。前提は nginx 1.30.0（nginx.org のパッケージ、stream と http_v3 あり）。実装中に設計から変えた点は §11 にまとめた。利用者向けの説明は README.md にある。

---

## 1. パーサ選定の調査結果

| 項目 | 結果 |
|---|---|
| リポジトリ | `github.com/nginxinc/nginx-go-crossplane`。存在し、archived ではない |
| 最新版 | v0.4.89（2026-05-25 tag、commit `11d0d6b`） |
| ライセンス | Apache-2.0（配布物に NOTICE/LICENSE を同梱すれば問題なし） |
| 依存 | 実行時は標準ライブラリだけ（testify / x/tools はテストとコード生成用）。CGO 不要 |
| 行番号 | `Directive.Line` で保持。ファイル名は `Config.File` にある（ディレクティブ単位ではない） |
| include | 相対パスは**メイン設定のディレクトリ基準**で解決（`filepath.Join(configDir, …)`）。glob を検出したらソートして展開 |
| 対応ディレクティブ表 | OSS 1.24 / 1.26 / latest、N+ R30–R37、njs、lua、headers-more、geoip2、otel、App Protect。`map`/`geo`/`split_clients`/`types` などの本体は `IsMapBlockParameter` 付きで扱える |
| 秘密情報 | ファイルを開くのは `include` の対象だけ。`Open` フックを差し替えられるので、読んだファイルを全部記録できる（ハッシュにも使う） |

### 注意点（ラップして対処する）
1. **同じファイルは1回しかパースしない**（`included` map、ソースにも `TODO: handle files included from multiple contexts` とある）。2回目以降の include は `Includes []int` で同じ Config を参照するだけ。
   → `CombineConfigs` は使わず、自前の展開器（splicer）で `Includes` をたどって木を組み立てる。展開中は include スタックを持つので、**循環 include はここで `cycle` として検出**する（crossplane 自体は重複を除くだけで、エラーにはしない）。
2. **v0.4.89 は nginx 1.31.5 より前のもの**。1.31.5 で入った `location $variable`（predicate location）や、それ以降の新ディレクティブは表に無い可能性がある。
   → `ErrorOnUnknownDirectives=false`、`SkipDirectiveContextCheck=true`、`SkipDirectiveArgsCheck=true` にして**構文だけ**を crossplane に任せる。意味の検証は自前で行う（未知のディレクティブは `unsupported-directive` として記録）。
3. 構文エラーがあると、そのファイルの残りは失われることがある（`StopParsingOnError=false` でも同じ）。→ 部分成功（exit 1）として扱う。

**結論:** crossplane を採用（字句解析と構文解析だけ）。意味解析・include の展開・循環検出は自前で行う。将来不満が出ても、`Directive` 相当の内部型を挟んでおくので差し替えられる。

---

## 2. 全体構成

```
cmd/nginx-sitemap/main.go      CLI・終了コード
internal/load/                 crossplane 呼び出し、include 展開、循環検出、ファイル収集とハッシュ
internal/ast/                  内部 AST（Directive{Name, Args, Src{File,Line}, IncludeChain, Block}）
internal/model/                http/stream のモデル化（listen, server, location, upstream, map …）
internal/eval/                 変数の部分評価（§4）
internal/route/                server 選択と location 選択の順位付け、内部リダイレクトの辺の解決
internal/emit/{json,html,dot}  出力
internal/atomicfs/             一時ファイル → fsync → rename
schema/map.schema.json         JSON Schema（draft 2020-12）
testdata/fixtures/<case>/      nginx.conf ほか + golden/{map.json,errors.json}
deploy/systemd/, deploy/nginx/ unit ファイルと公開用 nginx 設定のサンプル
```

1回の実行の流れ: load → model → eval → route → emit → atomic rename。常駐せず、goroutine も最小限にする。
メモリは `debug.SetMemoryLimit(40MiB)` を保険として入れ、AST は1回だけ保持する。数百 server 規模なら 20MB 前後の見込み（実装後に計測して報告する）。

---

## 3. データモデル（map.json）

```jsonc
{
  "schema_version": "1",
  "generated_at": "2026-11-07T12:00:00Z",
  "tool_version": "…",
  "config_hash": "sha256:…",          // §5
  "nginx": { "main": "/etc/nginx/nginx.conf", "conf_prefix": "/etc/nginx", "assumed_version": "1.31.8" },
  "files": [{ "path": "…", "sha256": "…", "masked": false }],
  "http": {
    "listens": [ Listen ],             // address:port ごとにまとめる
    "upstreams": [ Upstream ],
    "maps": [ MapTable ], "geos": [ … ], "split_clients": [ … ]
  },
  "stream": { "listens": [ Listen ], "upstreams": [ … ], "maps": [ … ] },
  "errors": [ Diag ], "warnings": [ Diag ]
}
```

すべての要素が共通で持つフィールド:
```jsonc
"src":    { "file": "conf.d/a.conf", "line": 12, "include_chain": ["nginx.conf:30"] },
"status": "static" | "resolved" | "conditional" | "dynamic",
"reasons": [ { "code": "request-dependent", "detail": "$http_x_env" } ]   // static 以外のとき
```

主な型:
- **Listen**: `addr`, `port`, `family`(v4/v6/unix), `implicit`（補完値なら true とその根拠）, `ssl`, `http2`, `quic`, `proxy_protocol`, `default_server`{server_ref, explicit:bool}, `servers[]`（server_name の優先順位どおりに並べた ServerRef）
- **Server**: `id`, `names[]`{value, kind: exact|leading-wildcard|trailing-wildcard|regex|dot-shorthand|empty, rank}, `locations[]`, `rewrite_program`（server レベルの set/if/rewrite/return）
- **Location**: `id`, `modifier`（`=`/`^~`/`~`/`~*`/prefix/`@`/predicate）, `uri`, `match_rank`（グループと出現順）, `children[]`, `handler`, `inherited[]`{directive, from: server|http|location:<id>, src}, `edges[]`
- **Handler**: `kind`（proxy_pass/fastcgi_pass/grpc_pass/uwsgi_pass/scgi_pass/memcached_pass/return/root/alias/try_files/static-default）, `target`（Expr）, `upstream_ref?`, `dns?`{mode: startup|runtime-resolver|upstream-resolve, snapshot?:{at, addrs[]}}, `implicit_slash_redirect`
- **Upstream**: `name`, `members[]`{server, weight, max_fails, fail_timeout, backup, down, resolve, max_conns, …}, `balancer`（least_conn/hash/ip_hash/random…）, `keepalive`, `zone`
- **Edge**（内部遷移）: `kind`（rewrite-last/rewrite-break/rewrite-redirect/error_page/try_files-fallback/if/return-redirect/X-Accel 不可視）, `condition`（Expr）, `target`（Expr）, `resolved_location?`, `status`
- **Expr**（変数評価の結果。§4）

JSON Schema は `schema/map.schema.json` として同梱し、テストで golden 全体をスキーマ検証する。

---

## 4. 変数評価の方針（実測しない部分評価）

### 4.1 値の表現
```
Expr = Lit(string)
     | Sym(var, reason)                          // リクエストに依存するなど、わからない値
     | Concat([Expr])
     | Switch(on: Expr, cases: [{match: Matcher, value: Expr}], default: Expr)
Matcher = exact | prefix-wildcard | suffix-wildcard | regex(pcre, ci) | cidr | range | percent(from,to) | if-cond
```
状態は Expr から機械的に決める:
- `static` — 変数を含まない
- `resolved` — 変数を含むが、すべて Lit に畳み込めた
- `conditional` — Switch が残るが、葉はすべて Lit
- `dynamic` — Sym が残る

### 4.2 規則
1. **set（定数伝播）**: rewrite モジュールのディレクティブ列（set/if/rewrite/return/break）を、**書かれた順に実行される命令列**として抽象実行する。環境は `var → Expr`。
   - server レベルの命令列は location 選択の前に実行される（`server` の rewrite フェーズ）。その結果の環境を location の命令列の初期値にする。
   - `if` の中の `set` は合流点で `Switch(if-cond, then, else)` にする。
   - `break` 以降は実行しない。`rewrite … last` は location を探し直す辺にする（nginx の上限は10回。こちらも同じ回数で打ち切る）。
2. **map**: 入力が Lit なら nginx と同じ順序（完全一致 → 長い前方ワイルドカード → 長い後方ワイルドカード（hostnames 指定時）→ 最初にマッチした正規表現 → default）で1つに決める。入力が未知なら Switch に展開する。値に変数が入っている場合や、map が連鎖している場合は再帰的に評価し、**訪問中の集合で循環を検出**する（`cycle`）。
3. **geo / split_clients**: 表をそのまま Switch にする（CIDR/範囲・割合）。入力（既定は `$remote_addr`、split_clients ならハッシュ元の文字列）は Sym。
4. **組込み変数**: §4.3 の表で、確定できるものと listen によって分岐するものを決める。
5. **リクエスト依存**: `$host` `$http_*` `$cookie_*` `$arg_*` `$request_uri` `$uri` `$1..$9`、名前付きキャプチャなどは `Sym(request-dependent)`。
6. **爆発防止**: Switch の葉が 256 を超えたら `dynamic(branch-limit)` に落とす。正規表現の評価は Go の regexp（RE2）でコンパイルできるものだけ行い、PCRE 固有の構文（後方参照・先読みなど）は `unsupported-regex` にする。

### 4.3 組込み変数（暫定。根拠は ngx_http_core_module の Embedded Variables）
| 変数 | 扱い | 根拠・補足 |
|---|---|---|
| `$server_port` | server の listen ポートが1つなら確定、複数なら listen ごとの Switch | "port of the server which accepted a request" |
| `$server_addr` | 明示 IP の listen なら確定、`*`/`[::]` なら Sym | "an address of the server which accepted a request" |
| `$scheme` / `$https` | listen の `ssl` の有無で確定、または Switch | "request scheme, http or https" / "on if connection operates in SSL mode" |
| `$server_name` | **Q5 を参照**（現状は server_name が1つのときだけ確定） | "name of the server which accepted a request" |
| `$document_root` | root/alias の Expr を代入する | "root or alias directive's value" |
| `$realpath_root` | Sym(`filesystem-dependent`) | symlink の解決が必要なため |
| `$hostname` | Sym(`host-dependent`) | 生成ホストと nginx ホストは同じでも、値は実行時のもの |
| `$nginx_version` / `$pid` | 前者は `--nginx-version` 指定時だけ確定、後者は Sym | |
| `$proxy_protocol_*` / `$remote_*` / `$ssl_*` / `$request_*` | Sym(request-dependent) | |

### 4.4 理由コード（一覧）
`request-dependent`, `connection-dependent`, `filesystem-dependent`, `host-dependent`, `runtime-dns`, `unsupported-directive`, `unsupported-regex`, `unsupported-module`（njs / perl / lua の変数）, `cycle`, `branch-limit`, `redirect-limit`, `unknown-variable`, `parse-error`, `masked`

---

## 5. 設定ハッシュとスキップ
`sha256(tool_version ‖ 正規化したオプション ‖ 読んだファイルを path 順にソートした (path, size, sha256) の列)`。
前回の `map.json` にある `config_hash` と同じで、`--skip-unchanged` が指定されていれば何も書かずに exit 0。
`--resolve` を使った実行は、DNS 結果が変わりうるので**スキップしない**。

## 6. 出力
- `map.json` / `index.html` / **`map.dot`（Graphviz）を採用**。Mermaid は既定で辺が 500 本までで、数百 server 規模だと描画できないため。
- `index.html` は単一ファイル。JSON を `<script type="application/json">` で埋め込み、検索・絞り込み・dynamic のハイライト・出典・エラー一覧・生成時刻・ハッシュを表示する。外部リソースは一切読み込まず、CSP は `default-src 'none'; script-src 'sha256-…'; style-src 'sha256-…'`。
- 書き込み: 出力先と同じディレクトリに `CreateTemp` → write → fsync → chmod 0644 → rename → ディレクトリを fsync。
- 終了コード: 0=成功 / 1=エラーはあったが出力した / 2=メイン設定が読めない・出力先に書けない・内部 panic。

## 7. systemd とパーミッション（方針）
- **DynamicUser は使わず、固定のシステムユーザ `nginx-sitemap` を推奨**します。DynamicUser と `StateDirectory=` を組み合わせると、実体が `/var/lib/private/`（0700 root）の下に置かれ、nginx ワーカー（www-data など）がたどれないためです。
- 読み取りに必要な権限: `/etc/nginx` 配下の**設定ファイル**に o+r（または `nginx-sitemap` を所有グループに入れる）。鍵や htpasswd の読み取り権限は**不要**です（開かないため）。
- unit の指定: `ProtectSystem=strict`、`ReadWritePaths=/var/lib/nginx-sitemap`、`ReadOnlyPaths=/etc/nginx`、`InaccessiblePaths=`（鍵ディレクトリ。任意で多重防御）、`PrivateNetwork=yes`（`--resolve` を使う場合は drop-in で解除）、`MemoryMax=64M`、`CPUQuota=50%`、`Nice=10`、`IOSchedulingClass=idle`、`UMask=0022`、`CapabilityBoundingSet=`、`SystemCallFilter=@system-service`。
- timer: `OnCalendar=*:0/15`、`Persistent=true`、`RandomizedDelaySec=60`。

---

## 8. 仕様解釈の決定事項

**基本方針: nginx 公式ドキュメントに記載のない点は、nginx のソースコードを根拠にする。**
根拠は、コメントと README の「仕様の根拠」表に `doc:<URL#anchor>` か `src:<path>:<function>`（参照した nginx バージョンも書く）として残す。ソースで裏付けが取れなかった点は推測で埋めず、`dynamic(unsupported)` として出力する。

| # | 論点 | 決定 |
|---|---|---|
| Q1 | include の相対パスの基準 | conf_prefix（`-c` で渡したファイルのディレクトリ）。根拠は src: `ngx_conf_full_name(…, 1)` |
| Q2 | ドキュメントにない点の根拠 | ソースコード（上の基本方針） |
| Q3 | `*:80` と明示アドレス `:80` の共存 | ソースの挙動に従う（`ngx_http_add_addresses` / `ngx_http_init_listening` まわりを確認して実装） |
| Q4 | listen 省略時の `*:80` と `*:8000` | ツールの設定ファイル（§10）の `assume_superuser on|off;` で指定する（既定 on）。補完値には仮定した内容を表示する |
| Q5 | `$server_name` | server_name が1つのときだけ確定させ、複数なら Sym |
| Q6 | rewrite 系ディレクティブのネストした location への継承 | ソースの挙動に従う（`ngx_http_rewrite_merge_loc_conf` を確認して実装） |
| Q7 | 表示する継承ディレクティブ | 暫定案のまま。§10 の設定で追加・削除できるようにする |
| Q8 | predicate location | 対象に含める |
| Q9 | PCRE と RE2 の差 | RE2 でコンパイルできる式だけ評価し、それ以外は `unsupported-regex` |
| Q10 | 設定中の秘密値のマスク | 暫定案どおり、既定でマスクする |
| Q11 | `--resolve` の解決器 | 純 Go リゾルバ（/etc/hosts と resolv.conf だけ）であることを README に明記する |

## 9. 環境情報（未記入のため必要）
1. OS / ディストリ（このマシンは WSL2 の Ubuntu 24.04 だが、配置先は？）
2. `nginx -V` の出力（バージョン、`--prefix`、`--conf-path`、組み込みモジュール。stream の有無、1.31.5 以上かどうか）
3. メイン設定のパス
4. 公開用のホスト名と listen IP:port、TLS 証明書の有無
5. 公開範囲の制御（allow/deny の CIDR、または既存の認証）
6. 出力先（暫定 `/var/lib/nginx-sitemap/`）と nginx ワーカーのユーザ名
7. **fixture の `nginx -t` 検証**: このマシンに nginx は入っていない。Docker 28.3 はあるので、公式イメージ `nginx:<本番と同じバージョン>` で `nginx -t` を流す `make verify-fixtures` を用意してよいか（go test 本体は Docker に依存させない）。
   - fixture の proxy_pass 先は、`nginx -t` が起動時に名前解決するので、IP か upstream 名だけを使う。

---

## 10. ツール設定ファイルと手動追記（nginx 設定に無いものの登録）

### 目的
共通基盤から独立させている監視系など、**このnginxを経由しないもの**も同じマップに載せる。
ユーザが自分で書いた内容だけを載せ、ツールが推測で何かを足すことはしない。

### 原則
- **出自を必ず区別する**: すべての要素に `origin: "nginx" | "manual"` を付ける。manual の出典は手動ファイルの `file:line`。HTML では別の色とバッジで表示する。
- **nginx 由来の事実は上書きしない**: manual は「追加」と「注記」だけ。nginx から導いた listen・server・location・backend は変更できない。
- **陳腐化を検出する**: 注記の対象（server_name・upstream 名など）が nginx 設定に見つからなければ warning（`stale-annotation`）。手動エントリに `reviewed 2026-11-01;` を書け、`stale_after 180d;` を過ぎたら warning にする。
- **ハッシュに含める**: 手動ファイルもハッシュに含めるので、追記すれば再生成される。
- **障害時に読めるようにする**: マップ自体は同じ nginx で配信されるため、nginx が落ちると Web からは見られない。`index.html` は単一ファイルで `file://` でも開けるので、README に「手元にコピーして閲覧する」手順を書く。

### 形式（推奨: nginx 風の構文）
crossplane でそのまま読めるため、**依存が増えず、行番号も取れ、nginx 運用者が見慣れた書き方**になる。
置き場所は `/etc/nginx-sitemap/sitemap.conf`（nginx の設定ツリーの外。nginx が誤って include しないようにする）。

```nginx
# ---- ツール設定 ----
settings {
    nginx_conf        /etc/nginx/nginx.conf;
    assume_superuser  on;              # Q4: listen 省略時に *:80 を仮定する
    nginx_version     1.31.8;          # $nginx_version、バージョン依存の構文判定
    output_dir        /var/lib/nginx-sitemap;
    skip_unchanged    on;
    resolve           off;
    mask_paths        /etc/nginx/private/*;
    mask_header       X-Api-Key;       # 既定（Authorization など）に追加
    inherited_show    proxy_read_timeout;
    stale_after       180d;
    include           /etc/nginx-sitemap/manual.d/*.conf;
}

# ---- 手動エントリ: nginx を経由しないサービス ----
external grafana-mon {
    group     monitoring;              # HTML ではグループ単位でまとめて表示
    endpoint  https://grafana.mon.internal:3000;   # 利用者がアクセスする入口
    listen    10.0.5.10:3000 ssl;
    backend   10.0.5.10:3000;
    depends   prometheus-mon;          # 手動エントリ同士の依存（辺として描画）
    owner     infra-mon;
    note      "共通基盤（このnginx）から独立。nginx 障害時の監視用";
    reviewed  2026-11-01;
}

# ---- 注記: nginx 由来の要素に情報を足す（中身は変えない）----
annotate server api.example.internal {
    owner  team-api;
    note   "外部 SaaS からの Webhook を受ける";
}
annotate upstream app_backend {
    note   "resolve 指定で実行時に解決。想定メンバは下記";
    expect 10.0.1.11:8080;             # status=declared として表示し、nginx 由来のメンバとは区別する
    expect 10.0.1.12:8080;
}
```

- map.json では、トップレベルに `external[]` を追加し、各要素に `annotations[]` を追加する。手動エントリの値は status `declared`（「ユーザが宣言した値」の意味。static/resolved とは区別する）。
- 未知のキーや書式誤りはエラー一覧に載せ（exit 1）、nginx 側の出力には影響させない。
- 代替案: YAML（`go.yaml.in/yaml/v3`。行番号は取れるが依存が1つ増える）。

---

## 11. 実装時に設計から変えた点

1. **include の展開は自前で行う。** crossplane は `SingleFile` モードでファイル単位に呼ぶだけにした。
   - 理由: crossplane は include の循環を見つけると、payload を返さずに全体を失敗させる（`configs contain include cycle`）。これでは「エラーがあっても出せる範囲で出力する」を満たせない。
   - 副次効果: 同じファイルを複数の文脈から include した場合も、それぞれの位置に正しく展開できる。
2. **構文エラーの復元を追加した。** crossplane は構文エラーのあるファイルを丸ごと捨てる。そこで字句解析のトークンから、エラー位置より前の完結した文だけを取り出し、開いたままのブロックを閉じて再パースする。
3. **`src` を文字列 `"file:line"` にし、include 経路は `files[].included_from` に一度だけ記録するようにした。** 理由: 800 server 規模で map.json が 17MB、RSS が 131MB になったため。
4. **出力は一時ファイルへ直接ストリーミングで書く。** 3 と合わせて、800 server / 5,600 location で RSS 43MB、map.json 5.2MB になった。
5. **map の連鎖は分岐ごとに合成する。** 例えば `$pool_addr` を `$host` の条件で直接表す。
6. **listen のパラメータを検証するようにした。** 1.30.0 で受け付けるもの以外は、nginx と同じく「invalid parameter」にする。
7. **循環 include の実機挙動を確認した。** nginx 1.30.0 の `nginx -t` は検出せず SIGSEGV で落ちる（Docker で確認）。
8. **JSON Schema は Go の型からリフレクションで生成する。** テストで schema ファイルとの一致を確認し、全ゴールデンをこのスキーマで検証する。
9. **手動エントリの書式は推奨案（nginx 風の構文）で実装した。** 項目は group / endpoint / listen / backend / depends / owner / note / tag / reviewed と、annotate の owner / note / tag / expect。
