(function () {
"use strict";
var M = JSON.parse(document.getElementById("data").textContent);
var P = M.portal || { sites: [], http_config: [], upstream_config: {} };
var NOW = Date.now();

// ================= helpers =================
function esc(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]; }); }
function isWeb(u) { return /^https?:\/\//i.test(u || ""); }
function ext(u, label) { return isWeb(u) ? "<a href='" + esc(u) + "' target='_blank' rel='noopener noreferrer'>" + esc(label || u) + "</a>" : "<code>" + esc(label || u) + "</code>"; }
function $(id) { return document.getElementById(id); }
function exprText(e) {
  if (!e) return "";
  switch (e.kind) {
    case "lit": return e.value;
    case "sym": return "${" + String(e.var).replace(/^\$/, "") + "}";
    case "concat": return (e.parts || []).map(exprText).join("");
    case "switch": return "⟨" + (e.table || "条件") + "⟩";
  }
  return "";
}
function daysLeft(iso) { return iso ? Math.floor((Date.parse(iso) - NOW) / 86400000) : null; }
function certSummary(site) {
  // the soonest expiry among the site's certificates
  if (!site.certificates || !site.certificates.length) return { kind: "none" };
  var best = null, err = null;
  site.certificates.forEach(function (c) {
    if (c.error) { err = c.error; return; }
    var d = daysLeft(c.not_after);
    if (best == null || d < best.d) best = { d: d, c: c };
  });
  if (best) return { kind: "ok", d: best.d, cert: best.c };
  return { kind: "err", error: err };
}
function certTag(cs) {
  if (!cs || cs.kind === "none") return "";
  if (cs.kind === "err") return "<span class='tag warn' title='" + esc(cs.error) + "'>証明書を読めない</span>";
  var d = cs.d;
  if (d < 0) return "<span class='tag bad'>期限切れ " + (-d) + " 日</span>";
  if (d <= 14) return "<span class='tag bad'>あと " + d + " 日</span>";
  if (d <= 30) return "<span class='tag warn'>あと " + d + " 日</span>";
  return "<span class='tag ok'>あと " + d + " 日</span>";
}
function certSoon(site) { var c = certSummary(site); return c.kind === "ok" && c.d <= 30; }
function certExpired(site) { var c = certSummary(site); return c.kind === "ok" && c.d < 0; }

// ================= data =================
var servers = {}, listens = {}, upstreams = {};
[M.http, M.stream].forEach(function (blk, i) {
  if (!blk) return;
  var mod = i === 0 ? "http" : "stream";
  (blk.servers || []).forEach(function (s) { servers[s.id] = s; });
  (blk.listens || []).forEach(function (l) { listens[l.id] = l; });
  (blk.upstreams || []).forEach(function (u) { upstreams[mod + ":" + u.name] = u; });
});
// manual entries become sites too
var externals = (M.external || []).map(function (x, i) {
  return { id: "ext-" + i, manual: true, name: x.name, url: (x.endpoints || []).filter(isWeb)[0] || "", endpoints: x.endpoints || [],
    group: x.group || "手動登録", owner: x.owner, note: x.note, links: x.links || [], inner_urls: x.backends || [], upstreams: [],
    listens: [], reviewed: x.reviewed, depends: x.depends || [], src: x.src, raw: x };
});
var sites = P.sites.concat(externals);
var siteById = {}; sites.forEach(function (s) { siteById[s.id] = s; });
var staleNames = {};
(M.warnings || []).forEach(function (w) { if (w.code === "stale-manual-entry") { var m = /"([^"]+)"/.exec(w.message); if (m) staleNames[m[1]] = true; } });
function upKey(site, name) { return (site.proto === "stream" ? "stream:" : "http:") + name; }
function listenLabel(id) {
  var l = listens[id]; if (!l) return id;
  var a = l.family === "unix" ? l.addr : l.addr + ":" + l.port;
  return (l.proto === "stream-tcp" ? "TCP " : l.proto === "stream-udp" ? "UDP " : "") + a;
}
function listenSub(id) {
  var l = listens[id]; if (!l) return "";
  var f = [l.ssl ? "https" : (l.proto === "http" ? "http" : "")];
  if (l.http2) f.push("http2"); if (l.quic) f.push("quic"); if (l.proxy_protocol) f.push("proxy_protocol");
  if (l.implicit) f.push("省略時の値");
  return f.filter(Boolean).join(" · ");
}

// ================= header =================
var errN = (M.errors || []).length, warnN = (M.warnings || []).length;
$("meta").innerHTML = "<span>生成 <code>" + esc(M.generated_at) + "</code></span><span>設定 <code>" + esc(String(M.config_hash).slice(0, 19)) + "…</code></span>" +
  (M.nginx.assumed_version ? "<span>nginx <code>" + esc(M.nginx.assumed_version) + "</code></span>" : "") + "<span>メイン設定 <code>" + esc(M.nginx.main) + "</code></span>";
if (errN) {
  $("errbar").hidden = false;
  $("errbar").innerHTML = "解析エラーが " + errN + " 件あります。表示が不完全な可能性があります。 <a id='toerr'>詳細を見る</a>";
  $("toerr").addEventListener("click", function () { show("diag"); });
  $("t-diag").textContent = "解析の詳細（エラー " + errN + "）";
}
var destSet = {}; sites.forEach(function (s) { (s.inner_urls || []).forEach(function (u) { destSet[u] = 1; }); });
var soonN = sites.filter(certSoon).length, expN = sites.filter(certExpired).length;
$("stats").innerHTML = "<span class='stat'><b>" + P.sites.length + "</b>サイト</span><span class='stat'><b>" + Object.keys(upstreams).length + "</b>upstream</span>" +
  "<span class='stat'><b>" + Object.keys(destSet).length + "</b>宛先</span><span class='stat'><b>" + externals.length + "</b>手動登録</span>" +
  "<span class='stat warn'><b>" + soonN + "</b>証明書 30 日以内</span><span class='stat bad'><b>" + expN + "</b>証明書 期限切れ</span>" +
  (errN ? "<span class='stat err'><b>" + errN + "</b>解析エラー</span>" : "") + (warnN ? "<span class='stat warn'><b>" + warnN + "</b>警告</span>" : "");

// ================= graph =================
var svg = $("svg"), NS = "http://www.w3.org/2000/svg", side = $("side"), crumbs = $("crumbs");
function el(tag, attrs, parent) { var e = document.createElementNS(NS, tag); for (var k in attrs) e.setAttribute(k, attrs[k]); parent.appendChild(e); return e; }
var colColor = ["--c-listen", "--c-group", "--c-path", "--c-up", "--c-be"];
function clip(s, w) { s = String(s); var max = Math.floor((w - 16) / 7.4); return s.length > max ? s.slice(0, Math.max(1, max - 1)) + "…" : s; }

function draw(cols, edges, titles, widths) {
  svg.innerHTML = "";
  var NH = 40, GAP = 10, TOP = 34, PADX = 16, CG = 44, xs = [], x = PADX;
  widths.forEach(function (w) { xs.push(x); x += w + CG; });
  var W = x - CG + PADX, maxRows = Math.max(1, Math.max.apply(null, cols.map(function (c) { return c.length; })));
  var H = TOP + maxRows * (NH + GAP) + 16;
  svg.setAttribute("viewBox", "0 0 " + W + " " + H); svg.setAttribute("width", W); svg.setAttribute("height", H);
  titles.forEach(function (t, i) { el("text", { x: xs[i], y: 18, "class": "colhead" }, svg).textContent = t; });
  var pos = {};
  cols.forEach(function (c, ci) {
    var total = c.length * (NH + GAP) - GAP, start = TOP + Math.max(0, (maxRows * (NH + GAP) - GAP - total) / 2);
    c.forEach(function (n, i) { pos[n.id] = { x: xs[ci], y: start + i * (NH + GAP), w: widths[ci], n: n, ci: ci }; });
  });
  var gE = el("g", {}, svg), gN = el("g", {}, svg), nodeEls = {}, edgeEls = [];
  edges.forEach(function (e) {
    var a = pos[e.a], b = pos[e.b]; if (!a || !b) return;
    var x1 = a.x + a.w, y1 = a.y + NH / 2, x2 = b.x, y2 = b.y + NH / 2, mx = (x1 + x2) / 2;
    var p = el("path", { d: "M" + x1 + "," + y1 + " C" + mx + "," + y1 + " " + mx + "," + y2 + " " + x2 + "," + y2, "class": "edge" + (e.dyn ? " dyn" : "") + (e.man ? " man" : "") }, gE);
    if (e.when) el("title", {}, p).textContent = e.when;
    edgeEls.push({ e: e, el: p });
  });
  Object.keys(pos).forEach(function (id) {
    var Q = pos[id], n = Q.n;
    var g = el("g", { "class": "node" + (n.manual ? " manual" : ""), transform: "translate(" + Q.x + "," + Q.y + ")", tabindex: "0", role: "button" }, gN);
    el("rect", { "class": "box", width: Q.w, height: NH, rx: 7 }, g);
    if (!n.manual) el("rect", { width: 3, height: NH - 12, x: 0, y: 6, fill: "var(" + colColor[Math.min(Q.ci, 4)] + ")" }, g);
    el("text", { x: 10, y: n.sub ? 17 : 24 }, g).textContent = clip(n.label, Q.w - (n.count != null ? 36 : 0));
    if (n.sub) el("text", { x: 10, y: 32, "class": "sub" }, g).textContent = clip(n.sub, Q.w + 8);
    if (n.count != null) el("text", { x: Q.w - 10, y: 24, "class": "count", "text-anchor": "end" }, g).textContent = n.count;
    if (n.flag) el("circle", { cx: Q.w - 8, cy: 8, r: 4, fill: "var(" + n.flag + ")" }, g);
    if (n.title) el("title", {}, g).textContent = n.title;
    nodeEls[id] = g;
    g.addEventListener("mouseenter", function () { hl(id); });
    g.addEventListener("mouseleave", function () { hl(null); });
    g.addEventListener("focus", function () { hl(id); });
    g.addEventListener("blur", function () { hl(null); });
    g.addEventListener("click", function () { if (n.onclick) n.onclick(); });
    g.addEventListener("keydown", function (ev) { if (ev.key === "Enter" && n.onclick) n.onclick(); });
  });
  function hl(id) {
    if (!id) { Object.keys(nodeEls).forEach(function (k) { nodeEls[k].classList.remove("dim"); }); edgeEls.forEach(function (x) { x.el.classList.remove("dim", "hl"); }); return; }
    var set = {}; set[id] = true;
    (function down(x) { edges.forEach(function (e) { if (e.a === x && !set[e.b]) { set[e.b] = true; down(e.b); } }); })(id);
    (function up(x) { edges.forEach(function (e) { if (e.b === x && !set[e.a]) { set[e.a] = true; up(e.a); } }); })(id);
    Object.keys(nodeEls).forEach(function (k) { nodeEls[k].classList.toggle("dim", !set[k]); });
    edgeEls.forEach(function (x) { var on = !!(set[x.e.a] && set[x.e.b]); x.el.classList.toggle("dim", !on); x.el.classList.toggle("hl", on); });
  }
  svg.parentNode.scrollTop = 0; svg.parentNode.scrollLeft = 0;
}

var groupNames = [];
var byGroup = {};
sites.forEach(function (s) { if (!byGroup[s.group]) { byGroup[s.group] = []; groupNames.push(s.group); } byGroup[s.group].push(s); });
groupNames.sort(function (a, b) {
  var ma = byGroup[a].every(function (s) { return s.manual; }), mb = byGroup[b].every(function (s) { return s.manual; });
  if (ma !== mb) return ma ? 1 : -1;
  return a < b ? -1 : a > b ? 1 : 0;
});

function overview() {
  crumbs.innerHTML = "全体 <span class='tag'>グループやサイトを選ぶと経路を表示します</span>";
  var lids = Object.keys(listens);
  var c0 = lids.map(function (id) {
    var n = P.sites.filter(function (s) { return s.listens.indexOf(id) >= 0; }).length;
    return { id: "L:" + id, label: listenLabel(id), sub: listenSub(id), count: n };
  });
  // At most MAXG groups are drawn; smaller ones fold into one node.
  var MAXG = 20, shown = groupNames.slice();
  if (shown.length > MAXG) {
    var bySize = groupNames.slice().sort(function (a, b) { return byGroup[b].length - byGroup[a].length || (a < b ? -1 : 1); });
    var keep = {}; bySize.slice(0, MAXG).forEach(function (g) { keep[g] = 1; });
    shown = groupNames.filter(function (g) { return keep[g]; });
  }
  var folded = groupNames.filter(function (g) { return shown.indexOf(g) < 0; });
  var c1 = shown.map(function (g) {
    var members = byGroup[g], manual = members.every(function (s) { return s.manual; });
    return { id: "G:" + g, label: g, sub: manual ? "手動で登録" : "サイト", count: members.length, manual: manual,
      flag: members.some(certSoon) ? "--warn" : null, onclick: function () { showGroup(g); } };
  });
  if (folded.length) {
    var fn = folded.reduce(function (n, g) { return n + byGroup[g].length; }, 0);
    c1.push({ id: "G:*more", label: "ほか " + folded.length + " グループ", sub: "サイト", count: fn, onclick: function () { showGroups(folded); } });
  }
  function gid(g) { return shown.indexOf(g) >= 0 ? "G:" + g : "G:*more"; }
  var use = {};
  P.sites.forEach(function (s) { s.upstreams.forEach(function (u) { var k = upKey(s, u); use[k] = (use[k] || 0) + 1; }); });
  var top = Object.keys(use).sort(function (a, b) { return use[b] - use[a]; }).slice(0, 14);
  var c2 = top.map(function (k) {
    var u = upstreams[k];
    return { id: "U:" + k, label: k.replace(/^http:/, ""), sub: u ? u.members.length + " 台 · " + u.balancer : "", count: use[k], onclick: function () { showUpstream(k); } };
  });
  var rest = Object.keys(use).length - top.length;
  if (rest > 0) c2.push({ id: "U:more", label: "ほか " + rest + " 件", sub: "台帳で一覧", onclick: function () { show("ledger"); } });
  var edges = [], edgeSeen = {};
  groupNames.forEach(function (g) {
    var ls = {}, us = {}, more = false;
    byGroup[g].forEach(function (s) { s.listens.forEach(function (l) { ls[l] = 1; }); s.upstreams.forEach(function (u) { us[upKey(s, u)] = 1; }); });
    Object.keys(ls).forEach(function (l) { pushEdge("L:" + l, gid(g)); });
    Object.keys(us).forEach(function (u) { if (top.indexOf(u) >= 0) pushEdge(gid(g), "U:" + u); else more = true; });
    if (more) pushEdge(gid(g), "U:more");
  });
  function pushEdge(a, b) { var k = a + "\u0000" + b; if (!edgeSeen[k]) { edgeSeen[k] = 1; edges.push({ a: a, b: b }); } }
  draw([c0, c1, c2], edges, ["待ち受け", "サイトのグループ", "よく使われる upstream"], [170, 250, 250]);
  side.innerHTML = "<h2>全体</h2><div class='kind'>グループをクリックすると、所属するサイトがここに並びます。</div>" +
    "<dl class='kv'><dt>サイト</dt><dd>" + P.sites.length + " 件</dd><dt>グループ</dt><dd>" + groupNames.length + " 件</dd><dt>手動登録</dt><dd>" + externals.length + " 件</dd>" +
    "<dt>証明書</dt><dd>30 日以内 " + soonN + " 件、期限切れ " + expN + " 件</dd></dl>" +
    "<div class='kind'>サイト名から探す</div><input type='search' id='gfind' list='sitenames' placeholder='サイト名' aria-label='サイトを探す'>";
  var f = $("gfind");
  f.addEventListener("change", function () { var s = findSite(f.value); if (s) showSite(s.id); });
}
function showGroups(list) {
  side.innerHTML = "<h2>ほかのグループ</h2><div class='kind'>" + list.length + " グループ。名前をクリックすると所属するサイトを表示します。</div>" +
    "<input type='search' id='gsfilter' placeholder='グループ名で絞り込み' aria-label='グループの絞り込み'><ul class='list' id='gslist'></ul>";
  function fill(q) {
    $("gslist").innerHTML = list.filter(function (g) { return !q || g.toLowerCase().indexOf(q) >= 0; }).slice(0, 300).map(function (g) {
      return "<li><a href='#' data-group='" + esc(g) + "'>" + esc(g) + "</a> <span class='empty'>" + byGroup[g].length + "</span></li>";
    }).join("");
  }
  fill("");
  $("gsfilter").addEventListener("input", function (e) { fill(e.target.value.trim().toLowerCase()); });
  $("gslist").addEventListener("click", function (e) { var g = e.target.getAttribute && e.target.getAttribute("data-group"); if (g) { e.preventDefault(); showGroup(g); } });
}
function findSite(name) { name = String(name).trim(); return sites.filter(function (s) { return s.name === name; })[0]; }
function siteListHTML(list) {
  return list.slice(0, 300).map(function (s) {
    return "<li><a data-site='" + esc(s.id) + "' href='#'>" + esc(s.name) + "</a> " + (certSoon(s) ? certTag(certSummary(s)) : "") + "</li>";
  }).join("") + (list.length > 300 ? "<li class='empty'>ほか " + (list.length - 300) + " 件（絞り込んでください）</li>" : "");
}
function bindSiteLinks(root) { root.addEventListener("click", function (e) { var id = e.target.getAttribute && e.target.getAttribute("data-site"); if (id) { e.preventDefault(); showSite(id); } }); }
function showGroup(g) {
  var members = byGroup[g] || [];
  side.innerHTML = "<h2>" + esc(g) + "</h2><div class='kind'>" + members.length + " 件。名前をクリックすると経路を表示します。</div>" +
    "<input type='search' id='gfilter' placeholder='このグループで絞り込み' aria-label='グループ内の絞り込み'><ul class='list' id='glist'></ul>";
  var list = $("glist");
  list.innerHTML = siteListHTML(members);
  $("gfilter").addEventListener("input", function (e) { var q = e.target.value.trim().toLowerCase(); list.innerHTML = siteListHTML(members.filter(function (s) { return s.name.toLowerCase().indexOf(q) >= 0; })); });
  bindSiteLinks(list);
}
function showUpstream(key) {
  var u = upstreams[key], name = key.replace(/^(http|stream):/, "");
  var users = P.sites.filter(function (s) { return s.upstreams.some(function (x) { return upKey(s, x) === key; }); });
  side.innerHTML = "<h2>" + esc(name) + "</h2><div class='kind'>upstream" + (u && u.kind === "implicit" ? "（proxy_pass に直接書かれた宛先）" : "") + "</div>" +
    (u ? "<dl class='kv'><dt>方式</dt><dd>" + esc(u.balancer) + "</dd><dt>メンバ</dt><dd>" + u.members.map(function (m) {
      var f = []; if (m.backup) f.push("backup"); if (m.down) f.push("down"); if (m.weight !== 1) f.push("weight " + m.weight);
      return "<code>" + esc(m.server) + "</code>" + f.map(function (x) { return " <span class='tag'>" + esc(x) + "</span>"; }).join("");
    }).join("<br>") + "</dd>" + (u.src ? "<dt>出典</dt><dd><code>" + esc(u.src) + "</code></dd>" : "") + "</dl>" : "") +
    "<div class='kind'>使っているサイト（" + users.length + "）</div><ul class='list' id='ulist'>" + siteListHTML(users) + "</ul>";
  bindSiteLinks($("ulist"));
}

// one site: listen → site → path → upstream → destination
function locLabel(l, parent) { var m = l.modifier === "prefix" ? "" : l.modifier + " "; return (parent ? parent + " › " : "") + m + l.uri; }
function handlerSub(h) {
  if (!h) return "";
  switch (h.kind) {
    case "static": return h.note === "alias" ? "alias" : "静的ファイル";
    case "return": return "return " + (h.code || "");
    case "redirect": return "リダイレクト " + (h.code || "");
    case "internal-redirect": return "内部リダイレクト";
    default: return h.kind;
  }
}
function showSite(id) {
  show("graph");
  var s = siteById[id]; if (!s) return;
  crumbs.innerHTML = "<a href='#' id='back'>全体</a> › <a href='#' id='bgrp'>" + esc(s.group) + "</a> › <b>" + esc(s.name) + "</b>";
  $("back").addEventListener("click", function (e) { e.preventDefault(); overview(); });
  $("bgrp").addEventListener("click", function (e) { e.preventDefault(); overview(); showGroup(s.group); });
  if (s.manual) {
    var bes = s.inner_urls.map(function (b, i) { return { id: "X:" + i, label: b, manual: true }; });
    draw([[{ id: "S", label: s.name, sub: "手動で登録", manual: true }], bes], bes.map(function (b) { return { a: "S", b: b.id, man: true }; }), ["サービス", "宛先"], [250, 220]);
    side.innerHTML = siteDetail(s);
    return;
  }
  var srv = servers[s.server_id] || {};
  var c0 = s.listens.map(function (l) { return { id: "L:" + l, label: listenLabel(l), sub: listenSub(l) }; });
  var c1 = [{ id: "S", label: s.name, sub: s.owner ? "担当 " + s.owner : (s.aliases && s.aliases.length ? "別名 " + s.aliases.length + " 件" : "") }];
  var c2 = [], c3 = [], c4 = [], edges = [], seen = {};
  s.listens.forEach(function (l) { edges.push({ a: "L:" + l, b: "S" }); });
  function addNode(col, n) { if (seen[n.id]) return; seen[n.id] = 1; col.push(n); }
  function addHandler(pid, h) {
    if (!h) return;
    var dyn = h.status === "dynamic" || h.status === "conditional";
    (h.backends || []).forEach(function (b) {
      var bdyn = dyn || b.status === "dynamic" || b.status === "conditional";
      if (b.upstream) {
        var k = upKey(s, b.upstream), u = upstreams[k];
        addNode(c3, { id: "U:" + k, label: b.upstream, sub: u ? (u.kind === "implicit" ? "直接指定" : u.balancer) : "", onclick: function () { showUpstream(k); } });
        edges.push({ a: pid, b: "U:" + k, dyn: bdyn, when: b.when });
        if (u) u.members.forEach(function (m) {
          var f = m.backup ? "backup" : m.down ? "down" : "";
          addNode(c4, { id: "D:" + m.server, label: m.server, sub: f, flag: m.status === "dynamic" ? "--dyn" : null, title: m.status === "dynamic" ? "実行時に名前を解決" : "" });
          edges.push({ a: "U:" + k, b: "D:" + m.server });
        });
      } else {
        addNode(c4, { id: "D:" + b.url, label: b.url, sub: b.kind === "dynamic" ? "実行時に決まる" : "", flag: b.kind === "dynamic" ? "--dyn" : null });
        edges.push({ a: pid, b: "D:" + b.url, dyn: bdyn, when: b.when });
      }
    });
    if (!(h.backends || []).length) {
      var t = h.target ? exprText(h.target) : "";
      if (h.kind === "static" || h.kind === "try_files") { addNode(c4, { id: "D:root:" + t, label: t || "html", sub: "ローカルのファイル" }); edges.push({ a: pid, b: "D:root:" + t }); }
      else if (h.kind === "return" || h.kind === "redirect") { var lbl = (h.code || "") + (t ? " " + t : ""); addNode(c4, { id: "D:ret:" + lbl, label: lbl, sub: h.kind === "redirect" ? "リダイレクト" : "応答を返す" }); edges.push({ a: pid, b: "D:ret:" + lbl, dyn: dyn }); }
      else if (h.kind === "internal-redirect") { addNode(c4, { id: "D:ir:" + t, label: "→ " + t, sub: "内部リダイレクト" }); edges.push({ a: pid, b: "D:ir:" + t }); }
    }
  }
  if (s.proto === "stream") {
    var h = srv.handler;
    c2.push({ id: "P:stream", label: h ? h.kind : "（処理なし）", sub: h && h.raw ? h.raw : "" });
    edges.push({ a: "S", b: "P:stream" });
    addHandler("P:stream", h);
  } else {
    if (srv.handler) { c2.push({ id: "P:srv", label: "（location に一致しない場合）", sub: handlerSub(srv.handler) }); edges.push({ a: "S", b: "P:srv" }); addHandler("P:srv", srv.handler); }
    (function walk(ls, parentLabel, parentId) {
      (ls || []).forEach(function (l) {
        var pid = "P:" + l.id, label = locLabel(l, parentLabel);
        var dyn = l.handler && (l.handler.status === "dynamic" || l.handler.status === "conditional");
        c2.push({ id: pid, label: label, sub: handlerSub(l.handler), flag: dyn ? "--dyn" : null, title: l.src || "" });
        edges.push({ a: parentId || "S", b: pid });
        addHandler(pid, l.handler);
        (l.if_blocks || []).forEach(function (ib, i) { if (ib.handler) { addHandler(pid, ib.handler); } });
        walk(l.children, label, null);
      });
    })(srv.locations, "", null);
  }
  draw([c0, c1, c2, c3, c4], edges, ["待ち受け", "サイト", "パス", "upstream", "宛先"], [150, 240, 220, 180, 220]);
  side.innerHTML = siteDetail(s);
  var cb = $("toconf"); if (cb) cb.addEventListener("click", function () { showConf(s.id); });
}
function siteDetail(s) {
  var h = "<h2>" + esc(s.name) + "</h2><div class='kind'>" + esc(s.group) + (s.manual ? " · 手動で登録" : s.proto === "stream" && s.group !== "TCP / UDP" ? " · TCP / UDP" : "") + "</div><dl class='kv'>";
  h += "<dt>外の URL</dt><dd>" + (s.url ? ext(s.url) : (s.endpoints && s.endpoints.length ? s.endpoints.map(function (u) { return ext(u); }).join("<br>") : "<span class='empty'>なし</span>")) + "</dd>";
  h += "<dt>中の URL</dt><dd>" + ((s.inner_urls || []).length ? s.inner_urls.map(function (u) { return ext(u); }).join("<br>") : "<span class='empty'>なし</span>") + "</dd>";
  if (s.certificates && s.certificates.length) {
    h += "<dt>証明書</dt><dd>" + s.certificates.map(function (c) {
      if (c.error) return "<span class='tag warn'>読めない</span> <code>" + esc(c.path) + "</code><div class='reason'>" + esc(c.error) + "</div>";
      return certTag({ kind: "ok", d: daysLeft(c.not_after) }) + " " + esc(String(c.not_after).slice(0, 10)) + " まで<div class='empty'>" + esc((c.dns_names || []).slice(0, 4).join(", ")) + "</div>";
    }).join("") + "</dd>";
  }
  if (s.note) h += "<dt>用途</dt><dd>" + esc(s.note) + "</dd>";
  if (s.owner) h += "<dt>担当</dt><dd>" + esc(s.owner) + "</dd>";
  if (s.access) h += "<dt>公開範囲</dt><dd>" + esc(s.access.summary) + (s.access.auth && s.access.auth.length ? "<br>" + s.access.auth.map(function (a) { return "<span class='tag'>" + esc(a) + "</span>"; }).join("") : "") + "</dd>";
  if (s.aliases && s.aliases.length) h += "<dt>別名</dt><dd>" + s.aliases.map(esc).join("<br>") + "</dd>";
  if (s.links && s.links.length) h += "<dt>リンク</dt><dd>" + s.links.map(function (l) { return ext(l.url, l.name); }).join("<br>") + "</dd>";
  if (s.manual && s.reviewed) h += "<dt>確認日</dt><dd>" + esc(s.reviewed) + (staleNames[s.name] ? " <span class='tag warn'>確認が古い</span>" : "") + "</dd>";
  if (s.manual && s.depends.length) h += "<dt>依存先</dt><dd>" + s.depends.map(esc).join("<br>") + "</dd>";
  if (s.reasons && s.reasons.length) h += "<dt>注意</dt><dd>" + s.reasons.map(function (r) { return "<div class='reason'>" + esc(r.code) + (r.detail ? ": " + esc(r.detail) : "") + "</div>"; }).join("") + "</dd>";
  h += "<dt>出典</dt><dd><code>" + esc(s.src || "") + "</code></dd></dl>";
  if (!s.manual) h += "<div><button class='btn' id='toconf' type='button'>このサイトの設定を見る</button></div>";
  return h;
}

// ================= site list =================
var groupsEl = $("groups"), sgroup = $("sgroup");
sgroup.innerHTML = "<option value=''>すべてのグループ（" + groupNames.length + "）</option>" + groupNames.map(function (g) { return "<option value='" + esc(g) + "'>" + esc(g) + "（" + byGroup[g].length + "）</option>"; }).join("");
sgroup.addEventListener("change", renderSites);
$("sq").addEventListener("input", renderSites);
function renderSites() {
  var q = $("sq").value.trim().toLowerCase(), g0 = sgroup.value, html = "";
  groupNames.forEach(function (g) {
    if (g0 && g !== g0) return;
    var list = byGroup[g].filter(function (s) { return !q || [s.name, s.note, s.owner, s.group, (s.aliases || []).join(" ")].join(" ").toLowerCase().indexOf(q) >= 0; });
    if (!list.length) return;
    html += "<div class='group'><h3>" + esc(g) + "（" + list.length + "）</h3><div class='cards'>" + list.map(card).join("") + "</div></div>";
  });
  groupsEl.innerHTML = html || "<div class='empty'>該当するサイトはありません。</div>";
}
function card(s) {
  var tags = [];
  if (s.manual) tags.push("<span class='tag man'>手動登録</span>");
  tags.push(certTag(certSummary(s)));
  if (s.access && s.access.summary !== "制限なし") tags.push("<span class='tag'>" + esc(s.access.summary) + "</span>");
  if (s.access && s.access.auth) s.access.auth.forEach(function (a) { tags.push("<span class='tag'>" + esc(a) + "</span>"); });
  if (s.status === "dynamic" || s.status === "conditional") tags.push("<span class='tag dyn'>条件で変わる</span>");
  if (s.manual && staleNames[s.name]) tags.push("<span class='tag warn'>確認が古い</span>");
  var open = s.url ? "<a class='open' href='" + esc(s.url) + "' target='_blank' rel='noopener noreferrer'>" + esc(s.url) + " ↗</a>" :
    (s.endpoints && s.endpoints.filter(isWeb).length ? "<a class='open' href='" + esc(s.endpoints.filter(isWeb)[0]) + "' target='_blank' rel='noopener noreferrer'>" + esc(s.endpoints.filter(isWeb)[0]) + " ↗</a>" : "<span class='empty'>開く URL なし</span>");
  return "<div class='card" + (s.manual ? " manual" : "") + "'><div class='name'>" + esc(s.name) + "</div>" + open +
    (s.note ? "<div class='note'>" + esc(s.note) + "</div>" : "") + "<div>" + tags.join("") + "</div>" +
    (s.links && s.links.length ? "<div class='acts'>" + s.links.map(function (l) { return isWeb(l.url) ? "<a href='" + esc(l.url) + "' target='_blank' rel='noopener noreferrer'>" + esc(l.name) + " ↗</a>" : ""; }).join("") + "</div>" : "") +
    "<div class='foot'>" + (s.owner ? "<span>担当 " + esc(s.owner) + "</span>" : "") + "</div>" +
    "<div class='acts'><a href='#' data-graph='" + esc(s.id) + "'>構成図で見る →</a>" + (s.manual ? "" : "<a href='#' data-conf='" + esc(s.id) + "'>設定を見る →</a>") + "</div></div>";
}
groupsEl.addEventListener("click", function (e) {
  var g = e.target.getAttribute && e.target.getAttribute("data-graph"), c = e.target.getAttribute && e.target.getAttribute("data-conf");
  if (g) { e.preventDefault(); showSite(g); }
  if (c) { e.preventDefault(); showConf(c); }
});

// ================= ledger =================
var ledger = $("ledger"), lmode = "site", lsort = "name";
$("lv-site").addEventListener("click", function () { lmode = "site"; this.setAttribute("aria-pressed", "true"); $("lv-be").setAttribute("aria-pressed", "false"); renderLedger(); });
$("lv-be").addEventListener("click", function () { lmode = "be"; this.setAttribute("aria-pressed", "true"); $("lv-site").setAttribute("aria-pressed", "false"); renderLedger(); });
$("lq").addEventListener("input", renderLedger);
$("lexp").addEventListener("change", renderLedger);
ledger.addEventListener("click", function (e) {
  var k = e.target.getAttribute && e.target.getAttribute("data-sort"); if (k) { lsort = k; renderLedger(); return; }
  var s = e.target.getAttribute && e.target.getAttribute("data-site"); if (s) { e.preventDefault(); showSite(s); }
});
function renderLedger() {
  var q = $("lq").value.trim().toLowerCase(), exp = $("lexp").checked;
  if (lmode === "site") {
    var rows = sites.filter(function (s) {
      if (exp && !certSoon(s)) return false;
      return !q || [s.name, s.url, s.owner, (s.inner_urls || []).join(" "), (s.endpoints || []).join(" ")].join(" ").toLowerCase().indexOf(q) >= 0;
    });
    rows.sort(function (a, b) {
      if (lsort === "cert") { var x = certSummary(a), y = certSummary(b); return (x.kind === "ok" ? x.d : 99999) - (y.kind === "ok" ? y.d : 99999); }
      return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
    });
    ledger.innerHTML = "<tr><th class='sort' data-sort='name'>サイト" + (lsort === "name" ? " ▲" : "") + "</th><th>外の URL</th><th>中の URL</th><th class='sort' data-sort='cert'>証明書の期限" + (lsort === "cert" ? " ▲" : "") + "</th><th>担当</th><th>リンク</th></tr>" +
      rows.map(function (s) {
        var cs = certSummary(s);
        return "<tr><td><a href='#' data-site='" + esc(s.id) + "'>" + esc(s.name) + "</a>" + (s.manual ? " <span class='tag man'>手動登録</span>" : "") + "</td>" +
          "<td>" + (s.url ? ext(s.url) : (s.endpoints || []).map(function (u) { return ext(u); }).join("<br>")) + "</td>" +
          "<td>" + (s.inner_urls || []).map(function (u) { return ext(u); }).join("<br>") + "</td>" +
          "<td class='num'>" + certTag(cs) + (cs.kind === "ok" ? "<br>" + esc(String(cs.cert.not_after).slice(0, 10)) : "") + "</td>" +
          "<td>" + esc(s.owner || "") + "</td><td>" + (s.links || []).map(function (l) { return ext(l.url, l.name); }).join("<br>") + "</td></tr>";
      }).join("");
  } else {
    var map = {};
    sites.forEach(function (s) { (s.inner_urls || []).forEach(function (u) { (map[u] = map[u] || []).push(s); }); });
    var keys = Object.keys(map).sort().filter(function (k) { return !q || (k + " " + map[k].map(function (s) { return s.name; }).join(" ")).toLowerCase().indexOf(q) >= 0; });
    ledger.innerHTML = "<tr><th>宛先</th><th>使っているサイト</th></tr>" + keys.map(function (k) {
      var list = map[k];
      return "<tr><td>" + ext(k) + "</td><td>" + list.slice(0, 8).map(function (s) { return "<a href='#' data-site='" + esc(s.id) + "'>" + esc(s.name) + "</a>"; }).join("<br>") +
        (list.length > 8 ? "<br><span class='empty'>ほか " + (list.length - 8) + " 件</span>" : "") + "</td></tr>";
    }).join("");
  }
}

// ================= configuration view =================
var confEl = $("conf");
$("sitenames").innerHTML = P.sites.map(function (s) { return "<option value='" + esc(s.name) + "'>"; }).join("");
$("cq").addEventListener("change", function (e) { var s = findSite(e.target.value); if (s && !s.manual) showConf(s.id); });
function hlCode(t) {
  var m = /^(\s*)(#.*)$/.exec(t); if (m) return esc(m[1]) + "<span class='tok-c'>" + esc(m[2]) + "</span>";
  m = /^(\s*)([A-Za-z_0-9]+)(.*)$/.exec(t);
  if (!m) return esc(t);
  return esc(m[1]) + "<span class='tok-d'>" + esc(m[2]) + "</span>" + esc(m[3]).replace(/(\$[A-Za-z_0-9]+)/g, "<span class='tok-v'>$1</span>");
}
function lineHTML(l) {
  var cls = l.kind === "include" ? "inc" : l.kind === "inherited" ? "inh" : l.kind === "marker" ? "mark" : "";
  return "<div class='ln " + cls + "'><span class='src' title='" + esc(l.src) + "'>" + esc(l.src) + "</span><span class='code'>" + hlCode(l.text) + "</span></div>";
}
function showConf(id) {
  show("conf");
  var s = siteById[id]; if (!s || s.manual) return;
  $("cq").value = s.name;
  var files = {}; s.config.forEach(function (l) { if (l.src) files[l.src.replace(/:\d+$/, "")] = 1; });
  var ups = s.upstreams.map(function (u) { return { name: u, lines: P.upstream_config[upKey(s, u)] }; }).filter(function (x) { return x.lines; });
  ups.forEach(function (u) { u.lines.forEach(function (l) { if (l.src) files[l.src.replace(/:\d+$/, "")] = 1; }); });
  var html = "<div class='head'><b>" + esc(s.name) + "</b><span class='empty'>" + Object.keys(files).length + " ファイル分をまとめて表示</span><a href='#' id='c2g'>構成図で見る →</a></div><pre>";
  if (s.proto === "http" && P.http_config.length) {
    html += "<div class='ln mark'><span class='src'></span><span class='code'># http レベルの設定（server に継承される）</span></div>" + P.http_config.map(lineHTML).join("");
  }
  html += s.config.map(lineHTML).join("");
  if (ups.length) {
    html += "<div class='ln mark'><span class='src'></span><span class='code'># このサイトが使う upstream</span></div>";
    ups.forEach(function (u) { html += u.lines.map(lineHTML).join(""); });
  }
  confEl.innerHTML = html + "</pre>";
  $("c2g").addEventListener("click", function (e) { e.preventDefault(); showSite(id); });
}

// ================= diagnostics =================
function diagTable(list, cls) {
  if (!list.length) return "<div class='empty'>ありません。</div>";
  return "<div class='tablewrap'><table><tr><th>種類</th><th>内容</th><th>出典</th></tr>" + list.map(function (d) {
    return "<tr><td class='" + cls + "'><code>" + esc(d.code) + "</code></td><td>" + esc(d.message) + "</td><td><code>" + esc(d.src || "") + "</code></td></tr>";
  }).join("") + "</table></div>";
}
$("tab-diag").innerHTML =
  "<h3 class='sec'>解析エラー（" + errN + "）</h3>" + diagTable(M.errors || [], "err") +
  "<h3 class='sec'>警告（" + warnN + "）</h3>" + diagTable(M.warnings || [], "warn") +
  "<h3 class='sec'>読み込んだファイル（" + (M.files || []).length + "）</h3><div class='tablewrap'><table><tr><th>ファイル</th><th>読み込み元</th><th>SHA-256</th></tr>" +
  (M.files || []).map(function (f) { return "<tr><td><code>" + esc(f.path) + "</code></td><td>" + (f.included_from || []).map(function (x) { return "<code>" + esc(x) + "</code>"; }).join("<br>") + "</td><td><code>" + esc(String(f.sha256).slice(0, 16)) + "</code></td></tr>"; }).join("") +
  "</table></div>" +
  "<h3 class='sec'>map / geo / split_clients</h3>" + tablesHTML() +
  "<p class='empty'>解析の全データは map.json にあります。</p>";
function tablesHTML() {
  var ts = [].concat(M.http ? M.http.tables : [], M.stream ? M.stream.tables : []);
  if (!ts.length) return "<div class='empty'>ありません。</div>";
  return ts.map(function (t) {
    return "<div class='tablewrap' style='margin-bottom:10px'><table><tr><th colspan='2'><code>" + esc(t.kind + " " + t.source + " $" + t.var) + "</code> " + esc(t.src || "") + "</th></tr>" +
      t.entries.map(function (e) { return "<tr><td><code>" + esc(e.match.kind + " " + e.match.pattern) + "</code></td><td><code>" + esc(e.value) + "</code></td></tr>"; }).join("") +
      (t.default ? "<tr><td>default</td><td><code>" + esc(t.default.value) + "</code></td></tr>" : "") + "</table></div>";
  }).join("");
}

// ================= tabs & routing =================
var tabs = document.querySelectorAll("#tabs button");
function show(t) {
  tabs.forEach(function (b) { b.setAttribute("aria-selected", b.getAttribute("data-tab") === t ? "true" : "false"); });
  ["graph", "sites", "ledger", "conf", "diag"].forEach(function (k) { $("tab-" + k).hidden = k !== t; });
  try { localStorage.setItem("nginx-sitemap-tab", t); } catch (e) {}
}
tabs.forEach(function (b) { b.addEventListener("click", function () {
  var t = b.getAttribute("data-tab"); show(t);
  if (t === "conf" && !confEl.innerHTML && P.sites.length) showConf(P.sites[0].id);
}); });
renderSites();
renderLedger();
overview();
var start = "graph";
try { start = localStorage.getItem("nginx-sitemap-tab") || "graph"; } catch (e) {}
show($("tab-" + start) ? start : "graph");
if (start === "conf" && P.sites.length) showConf(P.sites[0].id);
})();
