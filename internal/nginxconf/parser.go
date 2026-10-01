package nginxconf

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Directive is one statement: `name args…;` or `name args… { block }`.
// File and Line point at where it is written, so `inspect` can show the
// exact line. Unknown directives are kept as they are, never dropped.
type Directive struct {
	Name     string      `json:"name"`
	RawName  string      `json:"-"`               // source spelling of the name when quoted
	Args     []string    `json:"args,omitempty"`  // values, quotes removed
	Raw      []string    `json:"-"`               // source spelling of each arg
	Block    []Directive `json:"block,omitempty"` // children when HasBlock
	HasBlock bool        `json:"hasBlock,omitempty"`
	Opaque   string      `json:"opaque,omitempty"`   // raw body of a non-nginx block (lua)
	Included []string    `json:"included,omitempty"` // for include: the files it pulled in, in order
	File     string      `json:"file"`
	Line     int         `json:"line"`
}

// Pos is the directive's location as file:line.
func (d Directive) Pos() Pos { return Pos{File: d.File, Line: d.Line} }

// Arg returns argument i or "".
func (d Directive) Arg(i int) string {
	if i < len(d.Args) {
		return d.Args[i]
	}
	return ""
}

// Pos is a place in a config file.
type Pos struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

func (p Pos) String() string {
	if p.File == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", p.File, p.Line)
}

// Error is a parse or include problem at a file:line.
type Error struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Msg  string `json:"msg"`
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s in %s:%d", e.Msg, e.File, e.Line)
	}
	return fmt.Sprintf("%s in %s", e.Msg, e.File)
}

// FileInfo is what reading one file noticed about its bytes (CONF-10).
type FileInfo struct {
	Path    string `json:"path"`
	Link    string `json:"link,omitempty"` // symlink target (sites-enabled → sites-available)
	BOM     bool   `json:"bom,omitempty"`
	CRLF    bool   `json:"crlf,omitempty"`
	NonUTF8 bool   `json:"nonUtf8,omitempty"`
	// Managed names the tool whose header the file carries ("ngitool",
	// "edge"); hand-written files have none and are never modified (CONF-09).
	Managed string `json:"managed,omitempty"`
}

var managedRE = regexp.MustCompile(`(?m)^#\s*Managed by (NgiTool|edge)\b`)

// managedBy reads the header comment in a file's first lines.
func managedBy(src []byte) string {
	head := src
	if len(head) > 512 {
		head = head[:512]
	}
	if m := managedRE.FindSubmatch(head); m != nil {
		return strings.ToLower(string(m[1]))
	}
	return ""
}

// opaque reports whether a block's body is not nginx syntax and must be kept
// as raw text. Every openresty `*_by_lua_block` is.
func opaque(name string) bool { return strings.HasSuffix(name, "_by_lua_block") }

// Parse reads one file. It never gives up at the first problem: what can be
// parsed is returned together with every error found, so a broken config
// is still shown (CONF-03).
func Parse(file string, src []byte) ([]Directive, FileInfo, []*Error) {
	info := FileInfo{
		Path:    file,
		BOM:     len(src) >= 3 && string(src[:3]) == string(bom),
		CRLF:    strings.Contains(string(src), "\r\n"),
		NonUTF8: !utf8.Valid(src),
		Managed: managedBy(src),
	}
	p := &parser{lx: newLexer(file, src)}
	ds := p.block(0)
	return ds, info, p.errs
}

type parser struct {
	lx   *lexer
	errs []*Error
}

// block parses directives until `}` (depth > 0) or EOF.
func (p *parser) block(depth int) []Directive {
	var out []Directive
	var cur *Directive
	for {
		t, err := p.lx.next()
		if err != nil {
			p.errs = append(p.errs, err)
			return out
		}
		switch t.kind {
		case tokEOF:
			if cur != nil {
				p.errs = append(p.errs, p.lx.errf(cur.Line, "unexpected end of file, expecting \";\" or \"}\""))
				out = append(out, *cur)
			}
			if depth > 0 {
				p.errs = append(p.errs, p.lx.errf(t.line, "unexpected end of file, expecting \"}\""))
			}
			return out
		case tokWord:
			if cur == nil {
				cur = &Directive{Name: t.text, File: p.lx.file, Line: t.line}
				if t.quote != 0 {
					cur.RawName = t.raw
				}
				continue
			}
			cur.Args = append(cur.Args, t.text)
			cur.Raw = append(cur.Raw, t.raw)
		case tokSemi:
			if cur == nil {
				p.errs = append(p.errs, p.lx.errf(t.line, "unexpected \";\""))
				continue
			}
			out = append(out, *cur)
			cur = nil
		case tokOpen:
			if cur == nil {
				p.errs = append(p.errs, p.lx.errf(t.line, "unexpected \"{\""))
				cur = &Directive{File: p.lx.file, Line: t.line}
			}
			cur.HasBlock = true
			if opaque(cur.Name) {
				body, err := p.lx.rawBlock()
				if err != nil {
					p.errs = append(p.errs, err)
				}
				cur.Opaque = body
			} else {
				cur.Block = p.block(depth + 1)
			}
			if cur.Name != "" {
				out = append(out, *cur)
			}
			cur = nil
		case tokClose:
			if cur != nil {
				p.errs = append(p.errs, p.lx.errf(t.line, "unexpected \"}\""))
				out = append(out, *cur)
			}
			if depth == 0 {
				p.errs = append(p.errs, p.lx.errf(t.line, "unexpected \"}\""))
				cur = nil
				continue
			}
			return out
		}
	}
}

// Format writes directives back as nginx syntax, four spaces per level,
// with every argument spelled as it was read.
func Format(ds []Directive) string {
	var b strings.Builder
	format(&b, ds, 0)
	return b.String()
}

func format(b *strings.Builder, ds []Directive, depth int) {
	ind := strings.Repeat("    ", depth)
	for _, d := range ds {
		name := d.Name
		if d.RawName != "" {
			name = d.RawName
		}
		b.WriteString(ind + name)
		for i, a := range d.Args {
			raw := a
			if i < len(d.Raw) {
				raw = d.Raw[i]
			}
			b.WriteString(" " + raw)
		}
		switch {
		case d.HasBlock && d.Opaque != "":
			b.WriteString(" {" + d.Opaque + "}\n")
		case d.HasBlock:
			b.WriteString(" {\n")
			format(b, d.Block, depth+1)
			b.WriteString(ind + "}\n")
		default:
			b.WriteString(";\n")
		}
	}
}
