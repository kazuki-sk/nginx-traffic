(function () {
  "use strict";
  var M = JSON.parse(document.getElementById("data").textContent);

  function h(tag, attrs) {
    var el = document.createElement(tag);
    if (attrs) for (var k in attrs) {
      if (attrs[k] == null || attrs[k] === false) continue;
      if (k === "class") el.className = attrs[k];
      else if (k === "text") el.textContent = attrs[k];
      else el.setAttribute(k, attrs[k]);
    }
    for (var i = 2; i < arguments.length; i++) add(el, arguments[i]);
    return el;
  }
  function add(el, c) {
    if (c == null || c === false) return;
    if (Array.isArray(c)) { c.forEach(function (x) { add(el, x); }); return; }
    el.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
  }
  var chains = {};
  (M.files || []).forEach(function (f) { if (f.included_from && f.included_from.length) chains[f.path] = f.included_from.join("\n"); });
  function src(s) {
    if (!s) return null;
    var file = s.replace(/:\d+$/, "");
    return h("span", { class: "src", title: chains[file] ? "出典（include 経路）\n" + chains[file] : "出典" }, s);
  }
  var statusLabel = { static: "static", resolved: "resolved", conditional: "conditional", dynamic: "dynamic", declared: "manual" };
  function badge(st) { return st ? h("span", { class: "badge st-" + st }, statusLabel[st] || st) : null; }
  function reasons(rs) {
    if (!rs || !rs.length) return null;
    return h("div", { class: "reason" }, rs.map(function (r) { return h("div", null, "⚠ " + r.code + (r.detail ? " — " + r.detail : "")); }));
  }
  function exprText(e) {
    if (!e) return "";
    switch (e.kind) {
      case "lit": return e.value;
      case "sym": return "${" + e.var.replace(/^\$/, "") + "}";
      case "concat": return e.parts.map(exprText).join("");
      case "switch":
        var cs = (e.cases || []).map(function (c) { return matcherText(c.match) + " → " + exprText(c.value); });
        if (e.default) cs.push("default → " + exprText(e.default));
        return "⟨" + (e.table || exprText(e.on)) + ": " + cs.join(" | ") + "⟩";
    }
    return "";
  }
  function matcherText(m) {
    if (m.kind === "exact") return "= " + JSON.stringify(m.pattern);
    if (m.kind === "regex") return (m.ci ? "~* " : "~ ") + m.pattern;
    if (m.kind === "if") return "if " + m.pattern;
    return m.kind + " " + m.pattern;
  }
  function expr(e) { return e ? h("code", { class: "expr" }, exprText(e)) : null; }
  function isFlagged(st) { return st === "dynamic" || st === "conditional"; }

  var servers = {}, upstreams = {}, streamUp = {};
  (M.http && M.http.servers || []).forEach(function (s) { servers[s.id] = s; });
  (M.http && M.http.upstreams || []).forEach(function (u) { upstreams[u.name] = u; });
  (M.stream && M.stream.upstreams || []).forEach(function (u) { streamUp[u.name] = u; });

  // ---- counting ----
  var nLoc = 0, nDyn = 0;
  function countLocs(ls) { (ls || []).forEach(function (l) { nLoc++; if (isFlagged(l.status)) nDyn++; countLocs(l.children); }); }
  (M.http && M.http.servers || []).forEach(function (s) { countLocs(s.locations); if (isFlagged(s.status)) nDyn++; });

  // ---- header ----
  var hdr = document.getElementById("hdr");
  add(hdr.querySelector(".meta"), [
    h("span", null, "生成時刻 ", h("code", null, M.generated_at)),
    h("span", null, "設定ハッシュ ", h("code", null, M.config_hash)),
    h("span", null, "メイン設定 ", h("code", null, M.nginx.main)),
    M.nginx.assumed_version ? h("span", null, "nginx ", h("code", null, M.nginx.assumed_version)) : null,
    h("span", null, "listen 省略時の仮定 ", h("code", null, M.nginx.assume_superuser ? "superuser (*:80)" : "非 superuser (*:8000)")),
    h("span", null, "DNS ", h("code", null, M.dns.mode === "snapshot" ? "生成時点のスナップショット (" + M.dns.snapshot_at + ")" : "名前のまま")),
    h("span", null, "tool ", h("code", null, M.tool_version))
  ]);

  var stats = document.getElementById("stats");
  function stat(n, label, cls) { return h("div", { class: "stat" + (cls ? " " + cls : "") }, h("b", null, String(n)), h("span", null, label)); }
  add(stats, [
    stat((M.http ? M.http.listens.length : 0) + (M.stream ? M.stream.listens.length : 0), "listen"),
    stat((M.http ? M.http.servers.length : 0) + (M.stream ? M.stream.servers.length : 0), "server"),
    stat(nLoc, "location"),
    stat((M.http ? M.http.upstreams.length : 0) + (M.stream ? M.stream.upstreams.length : 0), "upstream"),
    stat(nDyn, "dynamic / conditional", nDyn ? "st-conditional" : ""),
    stat((M.external || []).length, "手動エントリ", "st-declared"),
    stat(M.errors.length, "エラー", M.errors.length ? "err" : ""),
    stat(M.warnings.length, "警告", M.warnings.length ? "warn" : "")
  ]);

  // ---- renderers ----
  function handlerView(hd, prefixUp) {
    if (!hd) return null;
    var rows = [h("span", null, "handler"), h("span", null, h("code", null, hd.kind), " ", badge(hd.status), " ", hd.raw ? h("code", { class: "expr" }, hd.raw) : null, " ", src(hd.src))];
    if (hd.target && (hd.kind !== "static" || hd.target.kind !== "lit")) rows.push(h("span", null, "評価結果"), h("span", null, expr(hd.target)));
    if (hd.note) rows.push(h("span", null, "注記"), h("span", null, hd.note));
    if (hd.backends && hd.backends.length) {
      rows.push(h("span", null, "宛先"), h("div", null, hd.backends.map(function (b) {
        var ups = prefixUp ? streamUp : upstreams;
        var u = b.upstream ? ups[b.upstream] : null;
        return h("div", { class: isFlagged(b.status) ? "dyn-row" : "" },
          b.when ? h("span", { class: "flag" }, "[" + b.when + "] ") : null,
          h("code", null, b.url), " ", badge(b.status), " ",
          b.upstream ? h("a", { href: "#up-" + (prefixUp || "") + b.upstream }, "upstream " + b.upstream) : h("span", { class: "flag" }, b.kind),
          b.dns ? h("span", { class: "flag" }, " DNS: " + b.dns) : null,
          b.snapshot ? h("span", { class: "flag" }, " スナップショット " + (b.snapshot.addrs || []).join(", ") + (b.snapshot.error ? " (" + b.snapshot.error + ")" : "")) : null,
          u ? h("div", { class: "flag" }, u.members.map(function (m) { return m.server + (m.backup ? " backup" : "") + (m.down ? " down" : "") + " w=" + m.weight; }).join(" / ")) : null,
          reasons(b.reasons));
      })));
    }
    if (hd.reasons && hd.reasons.length) rows.push(h("span", null, "理由"), reasons(hd.reasons));
    return h("div", { class: "kv" }, rows);
  }
  function edgesView(es) {
    if (!es || !es.length) return null;
    return h("table", null, h("tr", null, h("th", null, "遷移"), h("th", null, "条件"), h("th", null, "遷移先"), h("th", null, "状態"), h("th", null, "出典")),
      es.map(function (e) {
        return h("tr", { class: isFlagged(e.status) ? "dyn-row" : "" },
          h("td", null, h("code", null, e.kind + (e.code ? " " + e.code : ""))),
          h("td", null, e.condition || "（常に）"),
          h("td", null, expr(e.target), e.target_location ? [" → ", h("a", { href: "#" + e.target_location }, e.target_location)] : null, reasons(e.reasons)),
          h("td", null, badge(e.status)),
          h("td", null, src(e.src)));
      }));
  }
  function bindingsView(p) {
    if (!p || !p.bindings || !p.bindings.length) return null;
    return h("details", null, h("summary", null, "set（" + p.bindings.length + "）"),
      h("table", null, p.bindings.map(function (b) {
        return h("tr", { class: isFlagged(b.status) ? "dyn-row" : "" }, h("td", null, h("code", null, b.var)), h("td", null, expr(b.value)),
          h("td", null, b.under ? h("span", { class: "flag" }, "if " + b.under) : null), h("td", null, badge(b.status)), h("td", null, src(b.src)));
      })));
  }
  function inheritedView(inh) {
    if (!inh || !inh.length) return null;
    return h("details", null, h("summary", null, "有効なディレクティブ（継承を含む " + inh.length + "）"),
      h("table", null, inh.map(function (d) {
        return h("tr", null, h("td", null, h("code", null, d.directive)), h("td", null, h("code", null, d.args.join(" "))),
          h("td", { class: "flag" }, d.from === "here" ? "このレベル" : d.from === "default" ? "既定値" : d.from + " から継承"), h("td", null, src(d.src)));
      })));
  }
  function modText(m) { return m === "prefix" ? "" : m + " "; }
  function locView(l, withId) {
    var box = h("div", { class: "loc " + l.status, id: withId ? l.id : null, "data-search": (l.uri + " " + (l.handler && l.handler.raw || "")).toLowerCase() },
      h("div", { class: "loc-head" }, h("code", { class: "title" }, "location " + modText(l.modifier) + l.uri), h("span", { class: "flag" }, l.match_group + " #" + l.order),
        badge(l.status), l.auto_redirect ? h("span", { class: "badge flag" }, "末尾 / なしは 301") : null, src(l.src)),
      reasons(l.reasons),
      handlerView(l.handler),
      bindingsView(l.rewrite),
      l.if_blocks && l.if_blocks.length ? h("div", null, l.if_blocks.map(function (ib) {
        return h("div", { class: "kv" }, h("span", null, "if"), h("span", null, h("code", null, ib.condition), " ", badge(ib.status), " ", src(ib.src), reasons(ib.reasons), ib.handler ? handlerView(ib.handler) : null));
      })) : null,
      edgesView((l.rewrite && l.rewrite.edges || []).concat(l.edges || [])),
      inheritedView(l.inherited),
      (l.children || []).map(function (c) { return locView(c, withId); }));
    return box;
  }
  function serverView(s, listenId) {
    var names = s.names.map(function (n) { return n.name === "" ? '""' : n.name; }).join(" ");
    var card = h("details", { class: "card server", id: listenId ? null : s.id, "data-search": (names + " " + s.id).toLowerCase(), "data-status": s.status },
      h("summary", { class: "hd" }, h("span", { class: "title" }, s.id), h("code", null, names), badge(s.status), src(s.src)),
      h("div", { class: "bd" },
        (s.annotations || []).map(annView),
        reasons(s.reasons),
        h("div", { class: "kv" }, h("span", null, "listen"), h("span", null, s.listens.join(", "))),
        s.handler ? handlerView(s.handler) : null,
        bindingsView(s.rewrite),
        edgesView(s.rewrite && s.rewrite.edges),
        inheritedView(s.inherited),
        s.locations.length ? s.locations.map(function (l) { return locView(l, !listenId); }) : h("div", { class: "empty" }, "location なし")));
    return card;
  }
  function annView(a) {
    return h("div", { class: "card manual" }, h("div", { class: "hd" }, badge("declared"), h("span", null, "手動注記"), src(a.src)),
      h("div", { class: "bd kv" },
        a.owner ? [h("span", null, "owner"), h("span", null, a.owner)] : null,
        a.note ? [h("span", null, "note"), h("span", null, a.note)] : null,
        a.tags && a.tags.length ? [h("span", null, "tag"), h("span", null, a.tags.join(", "))] : null,
        a.expect && a.expect.length ? [h("span", null, "想定メンバ（宣言）"), h("span", null, a.expect.join(", "))] : null));
  }
  function listenView(l, isStream) {
    var flags = ["ssl", "http2", "quic", "proxy_protocol", "bind"].filter(function (f) { return l[f]; });
    var srvMap = isStream ? streamServers : servers;
    var card = h("div", { class: "card listen", id: "listen-" + l.id },
      h("div", { class: "hd" }, h("span", { class: "title" }, l.proto + "  " + l.addr + ":" + l.port), flags.map(function (f) { return h("span", { class: "badge flag" }, f); }),
        l.implicit ? h("span", { class: "badge st-resolved", title: l.implicit.basis }, "補完値 " + l.implicit.value) : null, badge(l.status), src(l.src)),
      h("div", { class: "bd" },
        l.implicit ? h("div", { class: "flag" }, "補完の根拠: " + l.implicit.basis) : null,
        l.socket ? h("div", { class: "flag" }, l.socket) : null,
        reasons(l.reasons),
        h("div", { class: "kv" }, h("span", null, "default_server"), h("span", null, l.default_server ? [h("a", { href: "#" + l.default_server.server_id }, l.default_server.server_id), l.default_server.explicit ? "（明示）" : "（暗黙: 最初の server）", " ", src(l.default_server.src)] : "—")),
        l.name_index && l.name_index.length ? h("details", null, h("summary", null, "server_name 選択順（" + l.name_index.length + "）"),
          h("table", null, h("tr", null, h("th", null, "順位"), h("th", null, "種別"), h("th", null, "名前"), h("th", null, "server"), h("th", null, "出典")),
            l.name_index.map(function (n) {
              return h("tr", { class: n.ignored ? "flag" : "" }, h("td", null, String(n.rank)), h("td", null, n.kind), h("td", null, h("code", null, n.name === "" ? '""' : n.name)),
                h("td", null, h("a", { href: "#" + n.server_id }, n.server_id), n.ignored ? " （無視: " + n.ignored + "）" : null), h("td", null, src(n.src)));
            }))) : null,
        l.servers.map(function (sid) { var s = srvMap[sid]; return s ? (isStream ? streamServerView(s, l.id) : serverView(s, l.id)) : null; })));
    return card;
  }
  var streamServers = {};
  (M.stream && M.stream.servers || []).forEach(function (s) { streamServers[s.id] = s; });
  function streamServerView(s, listenId) {
    var names = (s.names || []).map(function (n) { return n.name; }).join(" ");
    return h("details", { class: "card server", "data-search": (names + " " + s.id).toLowerCase(), "data-status": s.status },
      h("summary", { class: "hd" }, h("span", { class: "title" }, s.id), h("code", null, names), badge(s.status), src(s.src)),
      h("div", { class: "bd" }, (s.annotations || []).map(annView), handlerView(s.handler, "stream:"), inheritedView(s.inherited)));
  }
  function upstreamView(u, prefix) {
    return h("div", { class: "card", id: "up-" + (prefix || "") + u.name, "data-search": u.name.toLowerCase(), "data-status": u.status },
      h("div", { class: "hd" }, h("span", { class: "title" }, "upstream " + u.name), h("span", { class: "badge flag" }, u.kind), h("span", { class: "flag" }, u.balancer), badge(u.status), src(u.src)),
      h("div", { class: "bd" },
        (u.annotations || []).map(annView),
        reasons(u.reasons),
        h("table", null, h("tr", null, ["server", "weight", "max_fails", "fail_timeout", "max_conns", "backup", "down", "resolve", "その他", "出典"].map(function (x) { return h("th", null, x); })),
          u.members.map(function (m) {
            return h("tr", { class: isFlagged(m.status) ? "dyn-row" : "" }, h("td", null, h("code", null, m.server),
              m.snapshot ? h("div", { class: "flag" }, "スナップショット: " + (m.snapshot.addrs || []).join(", ") + (m.snapshot.error || "")) : null),
              h("td", null, String(m.weight)), h("td", null, String(m.max_fails)), h("td", null, m.fail_timeout), h("td", null, String(m.max_conns)),
              h("td", null, m.backup ? "yes" : ""), h("td", null, m.down ? "yes" : ""), h("td", null, m.resolve ? "yes" : ""),
              h("td", null, (m.params || []).join(" ")), h("td", null, src(m.src)));
          })),
        u.directives && u.directives.length ? h("div", { class: "flag" }, u.directives.map(function (d) { return d.directive + " " + d.args.join(" "); }).join("; ")) : null,
        h("div", { class: "flag" }, "使用箇所: ", u.used_by.length ? u.used_by.map(function (id) { return [h("a", { href: "#" + id }, id), " "]; }) : "なし")));
  }
  function tableView(t) {
    return h("div", { class: "card", "data-search": ("$" + t.var + " " + t.source).toLowerCase(), "data-status": t.status },
      h("div", { class: "hd" }, h("span", { class: "title" }, t.kind + " " + t.source + " $" + t.var), t.hostnames ? h("span", { class: "badge flag" }, "hostnames") : null,
        t.volatile ? h("span", { class: "badge flag" }, "volatile") : null, badge(t.status), src(t.src)),
      h("div", { class: "bd" }, reasons(t.reasons),
        h("table", null, h("tr", null, h("th", null, "条件"), h("th", null, "値"), h("th", null, "出典")),
          t.entries.map(function (e) { return h("tr", null, h("td", null, h("code", null, matcherText(e.match))), h("td", null, h("code", null, e.value)), h("td", null, src(e.src))); }),
          t.default ? h("tr", null, h("td", null, "default"), h("td", null, h("code", null, t.default.value)), h("td", null, src(t.default.src))) : null)));
  }
  function externalView(e) {
    return h("div", { class: "card manual", id: "ext-" + e.name, "data-search": (e.name + " " + (e.endpoints || []).join(" ") + " " + (e.group || "")).toLowerCase(), "data-status": "declared" },
      h("div", { class: "hd" }, h("span", { class: "title" }, e.name), e.group ? h("span", { class: "badge flag" }, e.group) : null, badge("declared"), src(e.src)),
      h("div", { class: "bd kv" },
        kvList("endpoint", e.endpoints), kvList("listen", e.listens), kvList("backend", e.backends),
        e.depends && e.depends.length ? [h("span", null, "depends"), h("span", null, e.depends.map(function (d) { return [h("a", { href: "#ext-" + d }, d), " "]; }))] : null,
        e.owner ? [h("span", null, "owner"), h("span", null, e.owner)] : null,
        e.note ? [h("span", null, "note"), h("span", null, e.note)] : null,
        e.reviewed ? [h("span", null, "reviewed"), h("span", null, e.reviewed)] : null,
        e.tags && e.tags.length ? [h("span", null, "tag"), h("span", null, e.tags.join(", "))] : null));
  }
  function kvList(k, xs) { return xs && xs.length ? [h("span", null, k), h("span", null, xs.map(function (x) { return h("div", null, h("code", null, x)); }))] : null; }
  function diagTable(ds, cls) {
    if (!ds.length) return h("div", { class: "empty" }, "なし");
    return h("table", null, h("tr", null, h("th", null, "コード"), h("th", null, "内容"), h("th", null, "出典")),
      ds.map(function (d) { return h("tr", null, h("td", { class: cls }, h("code", null, d.code)), h("td", null, d.message), h("td", null, src(d.src))); }));
  }

  // ---- sections ----
  var sections = {
    http: function () {
      if (!M.http) return h("div", { class: "empty" }, "http ブロックなし");
      return M.http.listens.map(function (l) { return listenView(l, false); });
    },
    stream: function () {
      if (!M.stream) return h("div", { class: "empty" }, "stream ブロックなし");
      return [M.stream.listens.map(function (l) { return listenView(l, true); }), h("h3", null, "stream upstream"), M.stream.upstreams.map(function (u) { return upstreamView(u, "stream:"); })];
    },
    upstreams: function () { return M.http ? M.http.upstreams.map(function (u) { return upstreamView(u, ""); }) : null; },
    tables: function () { return [].concat(M.http ? M.http.tables : [], M.stream ? M.stream.tables : []).map(tableView); },
    external: function () {
      if (!(M.external || []).length) return h("div", { class: "empty" }, "手動エントリなし（sitemap.conf の external で追加できます）");
      return M.external.map(externalView);
    },
    servers: function () { return M.http ? M.http.servers.map(function (s) { return serverView(s); }) : null; },
    diag: function () { return [h("h3", null, "エラー（" + M.errors.length + "）"), diagTable(M.errors, "err"), h("h3", null, "警告（" + M.warnings.length + "）"), diagTable(M.warnings, "warn"),
      h("h3", null, "読み込んだファイル（" + M.files.length + "）"), h("table", null, M.files.map(function (f) { return h("tr", null, h("td", null, h("code", null, f.path)), h("td", { class: "src" }, f.sha256.slice(0, 16)), h("td", { class: "src" }, f.size + " B")); }))]; }
  };
  var labels = { http: "HTTP listen", servers: "server 一覧", stream: "stream", upstreams: "upstream", tables: "map / geo / split", external: "手動エントリ", diag: "エラー・ファイル" };
  var tabs = document.getElementById("tabs"), main = document.getElementById("content");
  var panes = {};
  Object.keys(labels).forEach(function (k) {
    var pane = h("section", { class: "hidden", id: "pane-" + k }); add(pane, sections[k]()); panes[k] = pane; main.appendChild(pane);
    var label = labels[k] + (k === "diag" && (M.errors.length) ? " ⚠" + M.errors.length : "");
    var b = h("button", { type: "button", "data-k": k, "aria-selected": "false" }, label);
    b.addEventListener("click", function () { show(k); });
    tabs.appendChild(b);
  });
  function show(k) {
    Object.keys(panes).forEach(function (x) { panes[x].classList.toggle("hidden", x !== k); });
    Array.prototype.forEach.call(tabs.children, function (b) { b.setAttribute("aria-selected", b.getAttribute("data-k") === k ? "true" : "false"); });
    try { localStorage.setItem("nginx-sitemap-tab", k); } catch (e) {}
  }
  var initial = "http";
  try { initial = localStorage.getItem("nginx-sitemap-tab") || "http"; } catch (e) {}
  show(panes[initial] ? initial : "http");

  // ---- filtering ----
  var q = document.getElementById("q"), onlyDyn = document.getElementById("onlydyn");
  function filter() {
    var term = q.value.trim().toLowerCase(), dyn = onlyDyn.checked;
    document.querySelectorAll("[data-search]").forEach(function (el) {
      if (el.classList.contains("loc")) return;
      var txt = el.getAttribute("data-search") + " " + el.textContent.toLowerCase();
      var ok = (!term || txt.indexOf(term) >= 0) && (!dyn || isFlagged(el.getAttribute("data-status")) || el.querySelector(".loc.dynamic, .loc.conditional, .dyn-row"));
      el.classList.toggle("hidden", !ok);
      if (term && ok && el.tagName === "DETAILS") el.open = true;
    });
    document.querySelectorAll(".card.listen").forEach(function (c) {
      var vis = c.querySelectorAll(".server:not(.hidden)").length;
      c.classList.toggle("hidden", (term || dyn) && vis === 0);
    });
  }
  q.addEventListener("input", filter);
  onlyDyn.addEventListener("change", filter);
  function jump() {
    if (!location.hash) return;
    var t = document.getElementById(decodeURIComponent(location.hash.slice(1)));
    if (!t) return;
    var p = t.closest("section"); if (p) show(p.id.replace("pane-", ""));
    for (var d = t; d; d = d.parentElement) if (d.tagName === "DETAILS") d.open = true;
    t.scrollIntoView();
  }
  window.addEventListener("hashchange", jump);
  jump();
})();
