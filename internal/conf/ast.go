// Package conf loads an nginx configuration tree into an internal AST.
//
// Lexing/parsing is delegated to github.com/nginxinc/nginx-go-crossplane.
// Include expansion, include-cycle detection, glob semantics, file hashing
// and secret-file avoidance are implemented here (see load.go).
package conf

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Src is the origin of a directive. In JSON it is the string "file:line";
// the include chain of each file is recorded once in File.IncludedFrom.
type Src struct {
	File string
	Line int
	// Chain lists the include directives (outermost first) through which
	// this directive was reached, formatted "file:line". Not serialised.
	Chain []string
}

func (s Src) String() string { return fmt.Sprintf("%s:%d", s.File, s.Line) }

func (s Src) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func (s *Src) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	i := strings.LastIndexByte(str, ':')
	if i < 0 {
		s.File = str
		return nil
	}
	s.File = str[:i]
	n, err := strconv.Atoi(str[i+1:])
	s.Line = n
	return err
}

// Node is one directive after include expansion.
type Node struct {
	Name     string
	Args     []string
	Src      Src
	IsBlock  bool
	Block    []*Node
	Parent   *Node
	MapParam bool // body entry of a map-like block (map/geo/split_clients/types/...)
}

// Arg returns the i-th argument or "".
func (n *Node) Arg(i int) string {
	if i < len(n.Args) {
		return n.Args[i]
	}
	return ""
}

// Find returns the direct children with the given name.
func (n *Node) Find(name string) []*Node {
	var out []*Node
	for _, c := range n.Block {
		if c.Name == name && !c.MapParam {
			out = append(out, c)
		}
	}
	return out
}

// First returns the first direct child with the given name, or nil.
func (n *Node) First(name string) *Node {
	for _, c := range n.Block {
		if c.Name == name && !c.MapParam {
			return c
		}
	}
	return nil
}

// Last returns the last direct child with the given name, or nil.
// nginx reports "is duplicate" for most single-value directives, but for
// the ones where it does not, the last one wins at parse time.
func (n *Node) Last(name string) *Node {
	var last *Node
	for _, c := range n.Block {
		if c.Name == name && !c.MapParam {
			last = c
		}
	}
	return last
}

// Text renders the directive head (name + args) for display.
func (n *Node) Text() string {
	var b strings.Builder
	b.WriteString(n.Name)
	for _, a := range n.Args {
		b.WriteByte(' ')
		if a == "" || strings.ContainsAny(a, " \t;{}\"'") {
			b.WriteString(fmt.Sprintf("%q", a))
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}
