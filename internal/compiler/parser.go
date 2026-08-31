package compiler

import (
	"bytes"
	"fmt"
	"strings"

	frontmatterparse "github.com/adrg/frontmatter"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/frontmatter"
	"go.abhg.dev/goldmark/hashtag"
	"go.abhg.dev/goldmark/wikilink"
)

// LinkFact is a link occurrence emitted by the accepted Goldmark parser stack.
type LinkFact struct {
	Kind      string
	RawTarget string
	Display   string
	Fragment  string
	Embed     bool
	Ordinal   int
}

// TagFact is an inline hashtag occurrence. Tag excludes the leading '#'.
type TagFact struct {
	Tag     string
	Ordinal int
}

// SyntaxWarning is a non-fatal parser/frontmatter issue.
type SyntaxWarning struct {
	Code    string
	Message string
}

// Syntax is the structured syntax output consumed by resolver and graph code.
type Syntax struct {
	FrontmatterPresent bool
	Frontmatter        map[string]any
	Links              []LinkFact
	Tags               []TagFact
	Warnings           []SyntaxWarning
}

// Parser is the pinned, characterization-accepted Markdown parser stack. It
// owns syntax extraction; compiler code must not scan Markdown source itself.
type Parser struct {
	markdown goldmark.Markdown
	plain    goldmark.Markdown
}

func NewParser() *Parser {
	return &Parser{
		markdown: newGoldmark(true),
		plain:    newGoldmark(false),
	}
}

func newGoldmark(withFrontmatter bool) goldmark.Markdown {
	extenders := []goldmark.Extender{
		&mathExtender{},
		extension.Table,
		&hashtag.Extender{Variant: hashtag.ObsidianVariant},
		&wikilink.Extender{},
	}
	if withFrontmatter {
		extenders = append(extenders, &frontmatter.Extender{})
	}
	return goldmark.New(goldmark.WithExtensions(extenders...))
}

// Parse returns parser-owned syntax facts. The plain parser pass is used when
// the frontmatter extension reports no complete block: this preserves body
// parsing for an unmatched opening delimiter without a second Markdown lexer.
func (p *Parser) Parse(content []byte) (*Syntax, error) {
	if p == nil {
		p = NewParser()
	}
	result := &Syntax{Frontmatter: map[string]any{}}
	var metadata map[string]any
	body, frontmatterErr := frontmatterparse.Parse(bytes.NewReader(content), &metadata)
	if frontmatterErr == nil && !bytes.Equal(body, content) {
		result.FrontmatterPresent = true
		if metadata != nil {
			result.Frontmatter = metadata
		}
		root := p.plain.Parser().Parse(text.NewReader(body))
		return p.collect(root, body, result)
	}

	var root ast.Node
	if frontmatterErr != nil {
		// The frontmatter parser has already established that this is a
		// candidate block, but cannot return the body when YAML is malformed.
		// Goldmark's frontmatter extension preserves the body and exposes the
		// structured parse error through Data.Decode.
		ctx := parser.NewContext()
		root = p.markdown.Parser().Parse(text.NewReader(content), parser.WithContext(ctx))
		if data := frontmatter.Get(ctx); data != nil {
			result.FrontmatterPresent = true
			if err := data.Decode(&result.Frontmatter); err != nil {
				result.Warnings = append(result.Warnings, SyntaxWarning{Code: "frontmatter_malformed", Message: err.Error()})
			}
		}
	} else {
		root = p.plain.Parser().Parse(text.NewReader(content))
	}
	return p.collect(root, content, result)
}

func (p *Parser) collect(root ast.Node, content []byte, result *Syntax) (*Syntax, error) {
	if root == nil {
		return nil, fmt.Errorf("markdown parser returned nil root")
	}
	ordinal := 0
	err := ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *wikilink.Node:
			result.Links = append(result.Links, LinkFact{
				Kind: "wikilink", RawTarget: string(n.Target),
				Display: string(n.Text(content)), Fragment: string(n.Fragment),
				Embed: n.Embed, Ordinal: ordinal,
			})
			ordinal++
		case *ast.Image:
			result.Links = append(result.Links, LinkFact{
				Kind: "markdown_embed", RawTarget: string(n.Destination),
				Display: string(n.Text(content)), Embed: true, Ordinal: ordinal,
			})
			ordinal++
		case *ast.Link:
			result.Links = append(result.Links, LinkFact{
				Kind: "markdown", RawTarget: string(n.Destination),
				Display: string(n.Text(content)), Ordinal: ordinal,
			})
			ordinal++
		case *hashtag.Node:
			result.Tags = append(result.Tags, TagFact{Tag: string(n.Tag), Ordinal: ordinal})
			ordinal++
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// NormalizeTag applies the compiler's stable tag normalization contract.
func NormalizeTag(raw string) string {
	return strings.ToLower(strings.TrimPrefix(raw, "#"))
}

// IsExternalTarget identifies destinations that must not enter the local graph.
func IsExternalTarget(raw string) bool {
	return strings.HasPrefix(raw, "#") ||
		strings.HasPrefix(raw, "//") ||
		bytes.ContainsAny([]byte(raw), ":")
}
