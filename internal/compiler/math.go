package compiler

import (
	"bytes"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// MathInline is an opaque inline math region. Its source segment is retained
// for AST consumers, but its contents are intentionally not parsed as inline
// Markdown. Display is true when the node protects a paragraph-scoped $$...$$
// fallback that could not be promoted to a MathBlock by the paragraph
// transformer.
type MathInline struct {
	ast.BaseInline
	Segment text.Segment
	Display bool
}

var KindMathInline = ast.NewNodeKind("MathInline")

func (*MathInline) Kind() ast.NodeKind { return KindMathInline }

func (n *MathInline) IsRaw() bool { return true }

func (n *MathInline) Text(source []byte) []byte { return n.Segment.Value(source) }

func (n *MathInline) Dump(source []byte, level int) {
	attrs := map[string]string{"RawText": string(n.Text(source))}
	if n.Display {
		attrs["Display"] = "true"
	}
	ast.DumpHelper(n, source, level, attrs, nil)
}

// MathBlock is an opaque standalone display math region. It is created from a
// completed paragraph, after Goldmark has already established the surrounding
// block/container boundaries, so failed display candidates remain normal
// Markdown without block-parser lookahead.
type MathBlock struct {
	ast.BaseBlock
}

var KindMathBlock = ast.NewNodeKind("MathBlock")

func (*MathBlock) Kind() ast.NodeKind { return KindMathBlock }

func (*MathBlock) IsRaw() bool { return true }

func (n *MathBlock) Text(source []byte) []byte { return n.Lines().Value(source) }

func (n *MathBlock) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

type mathExtender struct{}

var _ goldmark.Extender = (*mathExtender)(nil)

func (*mathExtender) Extend(md goldmark.Markdown) {
	md.Parser().AddOptions(
		// Goldmark requires BlockParser.Open to stay on the current line. Use a
		// paragraph transformer for standalone display blocks instead of doing
		// multi-line preflight from BlockParser.Open.
		parser.WithParagraphTransformers(util.Prioritized(&mathParagraphTransformer{}, 50)),
		parser.WithInlineParsers(util.Prioritized(&mathInlineParser{}, 50)),
	)
}

// mathParagraphTransformer promotes one or more completed, leading standalone
// $$...$$ regions to raw MathBlock nodes before link-reference/table paragraph
// transformers run. If a closer is absent inside the already-established
// paragraph boundary, it changes nothing and normal Markdown parsing proceeds.
type mathParagraphTransformer struct{}

func (*mathParagraphTransformer) Transform(node *ast.Paragraph, reader text.Reader, _ parser.Context) {
	if node == nil || node.Parent() == nil {
		return
	}
	lines := node.Lines()
	source := reader.Source()
	parent := node.Parent()

	for lines.Len() > 0 {
		end := leadingDisplayMathEnd(lines, source)
		if end < 0 {
			return
		}

		block := &MathBlock{}
		block.Lines().AppendAll(lines.Sliced(0, end+1))
		parent.InsertBefore(parent, node, block)

		if end+1 == lines.Len() {
			parent.RemoveChild(parent, node)
			return
		}
		lines.SetSliced(end+1, lines.Len())
	}
}

func leadingDisplayMathEnd(lines *text.Segments, source []byte) int {
	if lines == nil || lines.Len() == 0 {
		return -1
	}
	firstSegment := lines.At(0)
	first := bytes.TrimSpace(firstSegment.Value(source))
	if len(first) < 2 || first[0] != '$' || first[1] != '$' || (len(first) > 2 && first[2] == '$') {
		return -1
	}

	if bytes.Equal(first, []byte("$$")) {
		for i := 1; i < lines.Len(); i++ {
			segment := lines.At(i)
			if bytes.Equal(bytes.TrimSpace(segment.Value(source)), []byte("$$")) {
				return i
			}
		}
		return -1
	}

	body := first[2:]
	close := bytes.Index(body, []byte("$$"))
	if close <= 0 || len(bytes.TrimSpace(body[close+2:])) != 0 {
		return -1
	}
	return 0
}

type mathInlineParser struct{}

func (*mathInlineParser) Trigger() []byte { return []byte{'$'} }

func (*mathInlineParser) Parse(_ ast.Node, reader text.Reader, _ parser.Context) ast.Node {
	line, start := reader.PeekLine()
	if len(line) < 2 || line[0] != '$' {
		return nil
	}
	if line[1] == '$' {
		return parseDisplayMathInline(reader, start)
	}
	return parseSingleDollarMath(reader, start)
}

// parseDisplayMathInline is the fallback for a display opener that begins at a
// line head but shares a Goldmark paragraph with preceding prose. InlineParser
// is allowed to read across the paragraph's BlockReader lines, so an unclosed
// candidate can restore the reader without crossing a block/container
// boundary or violating BlockParser.Open's contract.
func parseDisplayMathInline(reader text.Reader, start text.Segment) ast.Node {
	line, _ := reader.PeekLine()
	if reader.LineOffset() != 0 || len(line) < 2 || line[0] != '$' || line[1] != '$' ||
		(len(line) > 2 && line[2] == '$') {
		return nil
	}

	startLine, startPosition := reader.Position()
	if close := oneLineDisplayCloser(line); close >= 0 {
		reader.Advance(close + 2)
		_, end := reader.Position()
		return &MathInline{Segment: text.NewSegment(start.Start, end.Start), Display: true}
	}
	if !bytes.Equal(bytes.TrimSpace(line), []byte("$$")) {
		return nil
	}

	reader.AdvanceLine()
	for {
		line, _ = reader.PeekLine()
		if line == nil {
			reader.SetPosition(startLine, startPosition)
			return nil
		}
		if close := standaloneDisplayCloserOffset(line); close >= 0 {
			reader.Advance(close + 2)
			_, end := reader.Position()
			return &MathInline{Segment: text.NewSegment(start.Start, end.Start), Display: true}
		}
		reader.AdvanceLine()
	}
}

func oneLineDisplayCloser(line []byte) int {
	if len(line) < 4 || line[0] != '$' || line[1] != '$' || line[2] == '$' {
		return -1
	}
	for i := 2; i+1 < len(line); i++ {
		if line[i] != '$' || line[i+1] != '$' {
			continue
		}
		if i == 2 {
			return -1
		}
		if len(bytes.TrimSpace(line[i+2:])) == 0 {
			return i
		}
	}
	return -1
}

func standaloneDisplayCloserOffset(line []byte) int {
	if !bytes.Equal(bytes.TrimSpace(line), []byte("$$")) {
		return -1
	}
	return bytes.Index(line, []byte("$$"))
}

func parseSingleDollarMath(reader text.Reader, start text.Segment) ast.Node {
	line, _ := reader.PeekLine()
	if len(line) < 2 || line[0] != '$' || line[1] == '$' {
		return nil
	}
	// A dollar immediately before an ASCII digit is currency in the cases
	// covered by the syntax contract, not a safe math opener.
	if reader.PrecendingCharacter() == '$' || isWhitespacePrefix(line[1:]) || isASCIIDigit(line[1]) {
		return nil
	}

	startLine, startPosition := reader.Position()
	firstLine := true
	for {
		line, _ = reader.PeekLine()
		if line == nil {
			reader.SetPosition(startLine, startPosition)
			return nil
		}
		searchStart := 0
		if firstLine {
			searchStart = 1
		}
		if stop := findInlineMathCloser(line, searchStart); stop >= 0 {
			reader.Advance(stop + 1)
			_, end := reader.Position()
			return &MathInline{Segment: text.NewSegment(start.Start, end.Start)}
		}
		firstLine = false
		reader.AdvanceLine()
	}
}

func findInlineMathCloser(line []byte, start int) int {
	for i := start; i < len(line); i++ {
		if line[i] == '\\' {
			// Skip one escaped punctuation byte. Processing backslashes in pairs
			// keeps even/odd escape parity for sequences such as `\\$` and
			// `\\\$` without assigning Markdown code-span semantics inside math.
			if i+1 < len(line) && util.IsPunct(line[i+1]) {
				i++
			}
			continue
		}
		if line[i] != '$' {
			continue
		}
		if i == start || unicode.IsSpace(lastRune(line[:i])) {
			continue
		}
		if i+1 < len(line) && isASCIIDigit(line[i+1]) {
			continue
		}
		if line[i-1] == '$' || (i+1 < len(line) && line[i+1] == '$') {
			continue
		}
		return i
	}
	return -1
}

func lastRune(value []byte) rune {
	r, _ := utf8.DecodeLastRune(value)
	return r
}

func isASCIIDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func isWhitespacePrefix(value []byte) bool {
	r, _ := utf8.DecodeRune(value)
	return unicode.IsSpace(r)
}
