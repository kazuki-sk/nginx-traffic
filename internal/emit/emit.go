// Package emit renders a model.Map as map.json, index.html and map.dot.
package emit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"nginx-sitemap/internal/model"
)

// WriteJSON writes map.json (compact, deterministic field order).
func WriteJSON(w io.Writer, m *model.Map) error {
	return json.NewEncoder(w).Encode(m)
}

// JSON renders map.json into memory.
func JSON(m *model.Map) ([]byte, error) {
	var b bytes.Buffer
	err := WriteJSON(&b, m)
	return b.Bytes(), err
}

// WriteDOT writes map.dot.
func WriteDOT(w io.Writer, m *model.Map) error {
	_, err := w.Write(DOT(m))
	return err
}

// DOT renders a Graphviz graph: listen → server → location → backend/upstream.
// Graphviz was chosen over Mermaid because Mermaid's default limits
// (maxEdges 500, maxTextSize 50k) are exceeded by a few hundred servers.
func DOT(m *model.Map) []byte {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	q := func(s string) string {
		return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n").Replace(s) + "\""
	}
	color := func(status string) string {
		switch status {
		case model.StatusDynamic:
			return "#d9480f"
		case model.StatusConditional:
			return "#b08800"
		case model.StatusDeclared:
			return "#6f42c1"
		}
		return "#444444"
	}
	w("digraph nginx_sitemap {\n  rankdir=LR;\n  node [shape=box, fontname=\"monospace\", fontsize=10];\n  edge [fontname=\"monospace\", fontsize=9];\n")
	w("  label=%s; labelloc=t;\n", q("nginx sitemap  "+m.GeneratedAt+"  "+m.ConfigHash))
	ups := map[string]bool{}
	upNode := func(prefix, name string) string {
		id := prefix + "up:" + name
		if !ups[id] {
			ups[id] = true
			w("  %s [label=%s, shape=cylinder];\n", q(id), q("upstream "+name))
		}
		return id
	}
	backendEdges := func(from string, h *model.Handler, prefix string) {
		if h == nil {
			return
		}
		for _, be := range h.Backends {
			to := ""
			if be.Upstream != "" {
				to = upNode(prefix, be.Upstream)
			} else {
				to = prefix + "be:" + be.URL
				w("  %s [label=%s, shape=ellipse, color=%s];\n", q(to), q(be.URL), q(color(be.Status)))
			}
			label := h.Kind
			if be.When != "" {
				label += "\n" + be.When
			}
			w("  %s -> %s [label=%s, color=%s];\n", q(from), q(to), q(label), q(color(be.Status)))
		}
		if len(h.Backends) == 0 && (h.Kind == "return" || h.Kind == "redirect") {
			to := from + ":ret"
			lbl := fmt.Sprintf("%s %d", h.Kind, h.Code)
			if h.Target != nil {
				lbl += " " + h.Target.String()
			}
			w("  %s [label=%s, shape=note];\n  %s -> %s;\n", q(to), q(lbl), q(from), q(to))
		}
	}
	if m.HTTP != nil {
		for _, l := range m.HTTP.Listens {
			w("  %s [label=%s, shape=component, color=%s];\n", q(l.ID), q(listenLabel(l)), q(color(l.Status)))
			for _, sid := range l.Servers {
				attr := ""
				if l.DefaultServer != nil && l.DefaultServer.ServerID == sid {
					attr = ", style=bold, label=\"default\""
				}
				w("  %s -> %s [color=\"#888888\"%s];\n", q(l.ID), q(sid), attr)
			}
		}
		for _, s := range m.HTTP.Servers {
			w("  %s [label=%s, color=%s];\n", q(s.ID), q(s.ID+"\n"+serverNames(s.Names)), q(color(s.Status)))
			backendEdges(s.ID, s.Handler, "")
			var walk func(parent string, ls []*model.Location)
			walk = func(parent string, ls []*model.Location) {
				for _, loc := range ls {
					w("  %s [label=%s, color=%s];\n", q(loc.ID), q(strings.TrimSpace(modText(loc.Modifier)+" "+loc.URI)), q(color(loc.Status)))
					w("  %s -> %s [arrowhead=none, color=\"#bbbbbb\"];\n", q(parent), q(loc.ID))
					backendEdges(loc.ID, loc.Handler, "")
					for _, e := range loc.Edges {
						if e.TargetLocation != "" && e.TargetLocation != loc.ID {
							w("  %s -> %s [style=dashed, label=%s, color=%s];\n", q(loc.ID), q(e.TargetLocation), q(e.Kind), q(color(e.Status)))
						}
					}
					walk(loc.ID, loc.Children)
				}
			}
			walk(s.ID, s.Locations)
		}
		for _, u := range m.HTTP.Upstreams {
			id := upNode("", u.Name)
			for _, mb := range u.Members {
				mid := "m:" + u.Name + ":" + mb.Server
				lbl := mb.Server
				if mb.Backup {
					lbl += " (backup)"
				}
				if mb.Down {
					lbl += " (down)"
				}
				w("  %s [label=%s, shape=ellipse, color=%s];\n  %s -> %s [label=%s];\n", q(mid), q(lbl), q(color(mb.Status)), q(id), q(mid), q(fmt.Sprintf("w=%d", mb.Weight)))
			}
		}
	}
	if m.Stream != nil {
		for _, l := range m.Stream.Listens {
			w("  %s [label=%s, shape=component];\n", q(l.ID), q(listenLabel(l)))
			for _, sid := range l.Servers {
				w("  %s -> %s;\n", q(l.ID), q(sid))
			}
		}
		for _, s := range m.Stream.Servers {
			w("  %s [label=%s, color=%s];\n", q(s.ID), q(s.ID+"\n"+serverNames(s.Names)), q(color(s.Status)))
			backendEdges(s.ID, s.Handler, "stream:")
		}
		for _, u := range m.Stream.Upstreams {
			id := upNode("stream:", u.Name)
			for _, mb := range u.Members {
				mid := "stream:m:" + u.Name + ":" + mb.Server
				w("  %s [label=%s, shape=ellipse];\n  %s -> %s;\n", q(mid), q(mb.Server), q(id), q(mid))
			}
		}
	}
	if len(m.External) > 0 {
		groups := map[string][]model.External{}
		var keys []string
		for _, e := range m.External {
			if _, ok := groups[e.Group]; !ok {
				keys = append(keys, e.Group)
			}
			groups[e.Group] = append(groups[e.Group], e)
		}
		sort.Strings(keys)
		for i, g := range keys {
			w("  subgraph cluster_manual_%d {\n    label=%s; style=dashed; color=\"#6f42c1\";\n", i, q("manual: "+g))
			for _, e := range groups[g] {
				lbl := e.Name
				for _, ep := range e.Endpoints {
					lbl += "\n" + ep
				}
				w("    %s [label=%s, color=\"#6f42c1\", style=rounded];\n", q("ext:"+e.Name), q(lbl))
				for _, be := range e.Backends {
					w("    %s [label=%s, shape=ellipse, color=\"#6f42c1\"];\n    %s -> %s;\n", q("ext:"+e.Name+":"+be), q(be), q("ext:"+e.Name), q("ext:"+e.Name+":"+be))
				}
			}
			w("  }\n")
		}
		for _, e := range m.External {
			for _, d := range e.Depends {
				w("  %s -> %s [style=dotted, label=\"depends\", color=\"#6f42c1\"];\n", q("ext:"+e.Name), q("ext:"+d))
			}
		}
	}
	w("}\n")
	return []byte(b.String())
}

func listenLabel(l *model.Listen) string {
	s := fmt.Sprintf("%s %s:%d", l.Proto, l.Addr, l.Port)
	var f []string
	if l.SSL {
		f = append(f, "ssl")
	}
	if l.HTTP2 {
		f = append(f, "http2")
	}
	if l.QUIC {
		f = append(f, "quic")
	}
	if l.ProxyProtocol {
		f = append(f, "proxy_protocol")
	}
	if l.Implicit != nil {
		f = append(f, "implicit")
	}
	if len(f) > 0 {
		s += "\n" + strings.Join(f, " ")
	}
	return s
}

func serverNames(ns []model.ServerName) string {
	var out []string
	for _, n := range ns {
		if n.Name == "" {
			out = append(out, "\"\"")
		} else {
			out = append(out, n.Name)
		}
	}
	if len(out) > 4 {
		out = append(out[:4], fmt.Sprintf("(+%d)", len(out)-4))
	}
	return strings.Join(out, "\n")
}

func modText(m string) string {
	if m == "prefix" {
		return ""
	}
	return m
}
