package emit

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io"
	"strings"

	"nginx-sitemap/internal/model"
)

//go:embed assets/app.js
var appJS string

//go:embed assets/app.css
var appCSS string

// The page loads nothing external. CSP allows only the inline script/style
// by hash; the JSON data block is not executable.
var page = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{.CSP}}
<meta name="referrer" content="no-referrer">
<title>nginx-sitemap</title>
<style>{{.CSS}}</style>
</head>
<body>
<div class="errbar gut" id="errbar" hidden></div>
<header class="gut">
  <h1>nginx-sitemap</h1>
  <div class="meta" id="meta"></div>
  <nav class="tabs" id="tabs" aria-label="画面の切り替え">
    <button data-tab="graph" aria-selected="true">構成図</button>
    <button data-tab="sites" aria-selected="false">サイト一覧</button>
    <button data-tab="ledger" aria-selected="false">台帳</button>
    <button data-tab="conf" aria-selected="false">設定</button>
    <button data-tab="diag" aria-selected="false" id="t-diag">解析の詳細</button>
  </nav>
</header>
<main class="gut">
  <noscript>この画面の表示には JavaScript が必要です。データは map.json を参照してください。</noscript>
  <div class="stats" id="stats"></div>
  <section id="tab-graph">
    <div class="toolbar">
      <div class="crumbs" id="crumbs"></div>
      <div class="legend"><span><i></i>設定から確定</span><span><i class="dyn"></i>条件で変わる</span><span><i class="man"></i>手動で登録</span></div>
    </div>
    <div class="graphwrap">
      <div class="canvas"><svg id="svg" role="img" aria-label="構成図"></svg></div>
      <aside class="side" id="side"></aside>
    </div>
  </section>
  <section id="tab-sites" hidden>
    <div class="toolbar">
      <input type="search" id="sq" placeholder="サイト名・用途・担当で検索" aria-label="サイトの検索">
      <select id="sgroup" aria-label="グループ"></select>
    </div>
    <div class="groups" id="groups"></div>
  </section>
  <section id="tab-ledger" hidden>
    <div class="toolbar">
      <input type="search" id="lq" placeholder="サイト・URL・IP・担当で検索" aria-label="台帳の検索">
      <div class="seg"><button id="lv-site" aria-pressed="true">サイトごと</button><button id="lv-be" aria-pressed="false">宛先ごと</button></div>
      <label><input type="checkbox" id="lexp"> 証明書の期限が 30 日以内だけ</label>
    </div>
    <div class="tablewrap"><table id="ledger"></table></div>
  </section>
  <section id="tab-conf" hidden>
    <div class="toolbar">
      <input type="search" id="cq" list="sitenames" placeholder="サイト名を入力して設定を表示" aria-label="設定を表示するサイト">
      <datalist id="sitenames"></datalist>
      <span class="legend"><span class="tag">include した行は出典が青</span><span class="tag warn">http から継承した設定は黄色の背景</span></span>
    </div>
    <div class="conf" id="conf"></div>
  </section>
  <section id="tab-diag" hidden></section>
</main>
<script type="application/json" id="data">{{.Data}}</script>
<script>{{.JS}}</script>
</body>
</html>
`))

var htmlHead, htmlTail string

func init() {
	const marker = "\x00DATA\x00"
	var b bytes.Buffer
	if err := page.Execute(&b, map[string]any{
		"CSS":  template.CSS(appCSS),
		"JS":   template.JS(appJS),
		"Data": template.JS(marker),
		// Built outside the template so the base64 hashes are not entity-escaped.
		"CSP": template.HTML(`<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src '` +
			cspHash(appJS) + `'; style-src '` + cspHash(appCSS) + `'; img-src data:; base-uri 'none'; form-action 'none'">`),
	}); err != nil {
		panic(err)
	}
	htmlHead, htmlTail, _ = strings.Cut(b.String(), marker)
}

// WriteHTML streams index.html with the map embedded. json.Encoder escapes
// <, > and & (SetEscapeHTML defaults to true), so the data can never close
// the script element.
func WriteHTML(w io.Writer, m *model.Map) error {
	if _, err := io.WriteString(w, htmlHead); err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(m); err != nil {
		return err
	}
	_, err := io.WriteString(w, htmlTail)
	return err
}

func cspHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

// HTML renders index.html into memory.
func HTML(m *model.Map) ([]byte, error) {
	var b bytes.Buffer
	err := WriteHTML(&b, m)
	return b.Bytes(), err
}
