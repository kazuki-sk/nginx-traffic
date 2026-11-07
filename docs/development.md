# 開発者向けガイド

ビルド、テスト、リリースの手順をまとめる。
使い方は [README](../README.md) を参照。

## 必要なもの

- Go 1.27 系の最新のパッチ版
- Docker（fixture の検証に使う）
- make

Go の下限は 1.22。
nginx の正規表現でよく使う `(?<name>…)` を、Go の `regexp` が受け付けるのが 1.22 からだから。
動作を確認しているのは 1.27 系だけ。

標準ライブラリはバイナリに埋め込まれる。
Go にセキュリティ修正が出たら、ビルドし直してリリースする。

## ビルド

```bash
make build                 # bin/nginx-sitemap
make build GOARCH=arm64    # 別のアーキテクチャ向け
```

バイナリは CGO を使わない静的リンクになる。

## テスト

```bash
make test               # go test ./...
make verify-fixtures    # Docker の nginx:1.30.0 で fixture に nginx -t をかける
```

### ゴールデンテスト

`testdata/fixtures/<名前>/nginx.conf` を解析した結果を、同じディレクトリの `golden.map.json` と比べる。
`sitemap.conf` を置いた fixture では、それも読み込む。

出力を意図して変えた場合は、ゴールデンを作り直す。

```bash
make golden
```

差分を確認してからコミットする。

ゴールデンでは次の値を置き換えて比べる。

| 元の値 | 置き換え後 | 理由 |
|---|---|---|
| リポジトリの絶対パス | `<REPO>` | チェックアウト先で変わる |
| `config_hash` | `<HASH>` | ハッシュに絶対パスが含まれることがある |

ハッシュの挙動は `TestConfigHash` で別に確かめている。

### fixture の決まり

`testdata/fixtures/` には、`nginx -t` を通る設定だけを置く。
通らない設定は `testdata/invalid/` に置く。
`make verify-fixtures` は、前者が通り、後者が失敗することを確かめる。

fixture を書くときの注意:

- 静的な `proxy_pass` の宛先は、IP か upstream 名にする。`nginx -t` は名前を解決するので、存在しないホスト名だと失敗する。
- 証明書は `/etc/nginx-test/cert.pem` と `/etc/nginx-test/key.pem` を参照する。検証時にコンテナの中で作る。
- 内部 IP で待ち受ける設定も書ける。検証時はコンテナの中で `ip_nonlocal_bind` を有効にしている。

### スキーマ

`schema/map.schema.json` は、Go の型から生成している。
型を変えたら、`make golden` でスキーマも作り直す。
`TestSchemaUpToDate` は、スキーマが型と一致しているかを確かめる。
`TestGoldensMatchSchema` は、すべてのゴールデンがスキーマに合うかを確かめる。

## パッケージの構成

| パッケージ | 役割 |
|---|---|
| `cmd/nginx-sitemap` | コマンドライン |
| `internal/app` | 読み込みから出力までの流れ |
| `internal/conf` | 設定の読み込みと include の展開 |
| `internal/analyze` | 解析と変数の評価 |
| `internal/model` | `map.json` の型とスキーマの生成 |
| `internal/emit` | `map.json`、`index.html`、`map.dot` の出力 |
| `internal/sitecfg` | `sitemap.conf` の読み込み |
| `internal/atomicfs` | 一時ファイルを使った書き込み |
| `internal/schemacheck` | テスト用の JSON Schema 検証 |

`index.html` の画面は `internal/emit/assets/` にある。

## 配布物を作る

```bash
make dist                  # dist/nginx-sitemap-<version>-linux-<arch>.tar.gz
make dist GOARCH=arm64
```

tar.gz には次のものが入る。

- バイナリ
- `install.sh`
- systemd の unit、timer、sysusers の定義
- `sitemap.conf` と公開用 nginx 設定のサンプル
- README、設計書、スキーマ
- 依存ライブラリのライセンス

同じ場所に `.sha256` もできる。

## リリース

タグを push すると、GitHub Actions がリリースを作る。

```bash
git tag -a -s v0.1.0 -m "nginx-sitemap v0.1.0"
git push origin v0.1.0
```

ワークフローは `.github/workflows/release.yml`。
次の順に実行する。

1. gofmt と go vet
2. go test
3. fixture の nginx -t 検証
4. amd64 と arm64 の tar.gz の作成
5. tar.gz を一時ディレクトリに入れて動かす確認
6. GitHub Release の作成

タグに `-` を含む版は、pre-release として公開する。
`v0.1.0-rc1` や `v1.2.0-beta.1` が該当する。
pre-release は Latest にならない。

1 回の実行は 1 分ほどで終わる。

## ブランチとコミット

ブランチ名は、変更の種類で始める。

| 種類 | ブランチ名 |
|---|---|
| 機能追加 | `feature/<内容>` |
| 修正 | `fix/<内容>` |
| 緊急の修正 | `hotfix/<内容>` |
| ドキュメント | `docs/<内容>` |

`main` には PR の squash マージでだけ入れる。
コミットには検証済みの署名が要る。
SSH 鍵で署名する場合は、リポジトリで次のように設定する。

```bash
git config gpg.format ssh
git config user.signingkey ~/.ssh/id_ed25519.pub
git config commit.gpgsign true
```

使う鍵は、GitHub に署名用の鍵として登録しておく。
認証用として登録済みでも、署名用は別に登録が要る。
