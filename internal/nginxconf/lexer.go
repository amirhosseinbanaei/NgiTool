// Package nginxconf reads nginx configuration: a hand-written lexer and
// parser that follow nginx's own tokenizer (ngx_conf_read_token), the
// `nginx -T` dump splitter, the include resolver and the summariser that
// turns a parsed tree into servers, locations and upstreams. It has no
// dependency and never writes anything.
package nginxconf

import (
	"bytes"
	"fmt"
	"strings"
)

// tokKind is what a token is.
type tokKind int

const (
	tokEOF   tokKind = iota
	tokWord          // a directive name or argument, possibly quoted
	tokSemi          // ;
	tokOpen          // {
	tokClose         // }
)

type token struct {
	kind  tokKind
	text  string // the value: quotes removed, escapes applied
	raw   string // the source spelling, for writing it back
	quote byte   // '"' or '\'' when the token was quoted
	line  int
}

// lexer is pull-based so the parser can switch it to raw mode for an opaque
// block body (lua).
type lexer struct {
	src  []byte
	pos  int
	line int
	file string
}

func newLexer(file string, src []byte) *lexer {
	src = bytes.TrimPrefix(src, bom)
	return &lexer{src: src, line: 1, file: file}
}

var bom = []byte{0xEF, 0xBB, 0xBF}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func (l *lexer) errf(line int, format string, a ...any) *Error {
	return &Error{File: l.file, Line: line, Msg: fmt.Sprintf(format, a...)}
}

// next returns the next token. Comments are skipped. Like nginx, `#` starts
// a comment only at the start of a token, `}` does not end a bare word, and
// `{` is part of a word right after `$` (the `${var}` form).
func (l *lexer) next() (token, *Error) {
	for {
		for l.pos < len(l.src) && isSpace(l.src[l.pos]) {
			if l.src[l.pos] == '\n' {
				l.line++
			}
			l.pos++
		}
		if l.pos >= len(l.src) {
			return token{kind: tokEOF, line: l.line}, nil
		}
		if l.src[l.pos] != '#' {
			break
		}
		for l.pos < len(l.src) && l.src[l.pos] != '\n' {
			l.pos++
		}
	}
	c := l.src[l.pos]
	line := l.line
	switch c {
	case ';':
		l.pos++
		return token{kind: tokSemi, raw: ";", line: line}, nil
	case '{':
		l.pos++
		return token{kind: tokOpen, raw: "{", line: line}, nil
	case '}':
		l.pos++
		return token{kind: tokClose, raw: "}", line: line}, nil
	case '"', '\'':
		return l.quoted(c)
	}
	return l.word()
}

func (l *lexer) quoted(q byte) (token, *Error) {
	start, line := l.pos, l.line
	l.pos++
	var val strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == '\\' && l.pos+1 < len(l.src):
			n := l.src[l.pos+1]
			switch n {
			case '"', '\'', '\\':
				val.WriteByte(n)
			case 't':
				val.WriteByte('\t')
			case 'r':
				val.WriteByte('\r')
			case 'n':
				val.WriteByte('\n')
			default:
				val.WriteByte('\\')
				val.WriteByte(n)
			}
			if n == '\n' {
				l.line++
			}
			l.pos += 2
			continue
		case c == q:
			l.pos++
			return token{kind: tokWord, text: val.String(), raw: string(l.src[start:l.pos]), quote: q, line: line}, nil
		case c == '\n':
			l.line++
		}
		val.WriteByte(c)
		l.pos++
	}
	return token{}, l.errf(line, "unexpected end of file, expecting %q", string(q))
}

func (l *lexer) word() (token, *Error) {
	start, line := l.pos, l.line
	var val strings.Builder
	variable := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '{' && variable {
			val.WriteByte(c)
			l.pos++
			continue
		}
		variable = false
		switch {
		case c == '\\' && l.pos+1 < len(l.src):
			val.WriteByte(c)
			val.WriteByte(l.src[l.pos+1])
			if l.src[l.pos+1] == '\n' {
				l.line++
			}
			l.pos += 2
			continue
		case c == '$':
			variable = true
		case isSpace(c) || c == ';' || c == '{':
			return token{kind: tokWord, text: val.String(), raw: string(l.src[start:l.pos]), line: line}, nil
		}
		val.WriteByte(c)
		l.pos++
	}
	return token{kind: tokWord, text: val.String(), raw: string(l.src[start:l.pos]), line: line}, nil
}

// rawBlock reads an opaque block body after its `{` up to the matching `}`,
// skipping braces inside Lua strings, long brackets and comments, so an
// openresty `content_by_lua_block { … }` never breaks the parse (CONF-04).
func (l *lexer) rawBlock() (string, *Error) {
	start, line := l.pos, l.line
	depth := 1
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == '\n':
			l.line++
			l.pos++
		case c == '"' || c == '\'':
			l.pos++
			for l.pos < len(l.src) && l.src[l.pos] != c {
				if l.src[l.pos] == '\\' {
					l.pos++
				}
				if l.pos < len(l.src) && l.src[l.pos] == '\n' {
					l.line++
				}
				l.pos++
			}
			l.pos++
		case c == '[' && l.longBracket() >= 0:
			l.skipLong(l.longBracket())
		case c == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '-':
			l.pos += 2
			if lvl := l.longBracket(); lvl >= 0 {
				l.skipLong(lvl)
				continue
			}
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == '{':
			depth++
			l.pos++
		case c == '}':
			depth--
			if depth == 0 {
				body := string(l.src[start:l.pos])
				l.pos++
				return body, nil
			}
			l.pos++
		default:
			l.pos++
		}
	}
	return "", l.errf(line, "unexpected end of file in a raw block, expecting \"}\"")
}

// longBracket returns the level of a Lua long bracket ([[ or [==[) at pos,
// or -1.
func (l *lexer) longBracket() int {
	if l.pos >= len(l.src) || l.src[l.pos] != '[' {
		return -1
	}
	i := l.pos + 1
	for i < len(l.src) && l.src[i] == '=' {
		i++
	}
	if i < len(l.src) && l.src[i] == '[' {
		return i - l.pos - 1
	}
	return -1
}

func (l *lexer) skipLong(level int) {
	closing := "]" + strings.Repeat("=", level) + "]"
	l.pos += level + 2
	end := bytes.Index(l.src[l.pos:], []byte(closing))
	if end < 0 {
		end = len(l.src) - l.pos
	} else {
		end += len(closing)
	}
	l.line += bytes.Count(l.src[l.pos:l.pos+end], []byte("\n"))
	l.pos += end
}
