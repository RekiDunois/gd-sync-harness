package compiler

import (
	"bytes"
	"testing"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/hashtag"
	"go.abhg.dev/goldmark/wikilink"
)

func TestParserCharacterizationMandatoryFixtures(t *testing.T) {
	p := NewParser()
	valid, err := p.Parse([]byte("---\ntags: [Alpha, beta]\naliases: note\n---\n[[Target#Heading|shown]] ![[image.png]] [other](folder/other.md) #Inline\n```md\n[[fake]] #fake\n```\n`[[also-fake]] #also-fake`"))
	if err != nil {
		t.Fatal(err)
	}
	if !valid.FrontmatterPresent || len(valid.Links) != 3 || len(valid.Tags) != 1 {
		t.Fatalf("valid facts = %+v", valid)
	}
	if valid.Links[0].Kind != "wikilink" || valid.Links[0].Fragment != "Heading" || valid.Links[0].Display != "shown" {
		t.Fatalf("wikilink = %+v", valid.Links[0])
	}
	if !valid.Links[1].Embed || valid.Links[1].Kind != "wikilink" {
		t.Fatalf("embed = %+v", valid.Links[1])
	}
	if valid.Tags[0].Tag != "Inline" {
		t.Fatalf("tags = %+v", valid.Tags)
	}

	malformed, err := p.Parse([]byte("---\ntags: [broken\n---\n[[body-link]] #body-tag"))
	if err != nil {
		t.Fatal(err)
	}
	if !malformed.FrontmatterPresent || len(malformed.Warnings) != 1 || len(malformed.Links) != 1 || len(malformed.Tags) != 1 {
		t.Fatalf("malformed facts = %+v", malformed)
	}

	unmatched, err := p.Parse([]byte("---\nbody [[still-parsed]] #still-tag"))
	if err != nil {
		t.Fatal(err)
	}
	if unmatched.FrontmatterPresent || len(unmatched.Links) != 1 || len(unmatched.Tags) != 1 {
		t.Fatalf("unmatched facts = %+v", unmatched)
	}
}

func TestNormalizeTagAndExternalTarget(t *testing.T) {
	if got := NormalizeTag("#Topic/Sub"); got != "topic/sub" {
		t.Fatalf("normalized tag = %q", got)
	}
	for _, target := range []string{"https://example.invalid/note", "mailto:user@example.invalid", "#heading", "//example.invalid/note"} {
		if !IsExternalTarget(target) {
			t.Errorf("target %q classified local", target)
		}
	}
	if IsExternalTarget("folder/note.md") {
		t.Fatal("local target classified external")
	}
}

func TestParserMathProtectionCharacterization(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		links      int
		tags       int
		linkTarget string
	}{
		{name: "inline wikilink", content: "$[[fake]]$", links: 0, tags: 0},
		{name: "inline mixed", content: "$x #fake [[fake]]$", links: 0, tags: 0},
		{name: "multiline inline", content: "$x\n#fake [[fake]]$", links: 0, tags: 0},
		{name: "inline stops at blank line", content: "$x\n\n[[real]]", links: 1, tags: 0, linkTarget: "real"},
		{name: "one line display", content: "$$[[fake]]$$", links: 0, tags: 0},
		{name: "multiline display", content: "$$\n$x #fake [[fake]]\n$$\n[[real]]", links: 1, tags: 0, linkTarget: "real"},
		{name: "display after prose in same paragraph", content: "intro\n$$\n[[fake]] #fake\n$$\n[[real]]", links: 1, tags: 0, linkTarget: "real"},
		{name: "real link around inline math", content: "before [[real]] $[[fake]]$ after", links: 1, tags: 0, linkTarget: "real"},
		{name: "embed around inline math", content: "![[image.png]] $![[fake.png]]$", links: 1, tags: 0, linkTarget: "image.png"},
		{name: "currency prose", content: "pay $5 and [[real]] for $10", links: 1, tags: 0, linkTarget: "real"},
		{name: "currency before later math", content: "$20 and $30 then [[real]] and $x$", links: 1, tags: 0, linkTarget: "real"},
		{name: "unclosed inline rollback", content: "$[[text]] followed by [[real]]", links: 2, tags: 0, linkTarget: "text"},
		{name: "unclosed display rollback", content: "$$\nordinary [[real]]", links: 1, tags: 0, linkTarget: "real"},
		{name: "unclosed display cannot borrow list closer", content: "$$\nordinary [[real]]\n- $$\n  [[list-real]]", links: 2, tags: 0, linkTarget: "real"},
		{name: "unclosed display cannot borrow blockquote closer", content: "$$\nordinary [[real]]\n> $$\n> [[quote-real]]", links: 2, tags: 0, linkTarget: "real"},
		{name: "inline code owns contents", content: "`$[[fake]]$`", links: 0, tags: 0},
		{name: "fenced code owns contents", content: "```md\n$[[fake]]$ #fake\n```", links: 0, tags: 0},
		{name: "indented code owns contents", content: "    $$\n    [[fake]]\n    $$", links: 0, tags: 0},
		{name: "escaped dollars", content: `\$[[real]]\$`, links: 1, tags: 0, linkTarget: "real"},
		{name: "adjacent display blocks", content: "$$\n[[fake-a]] #fake-a\n$$\n$$\n[[fake-b]] #fake-b\n$$\n[[real]]", links: 1, tags: 0, linkTarget: "real"},
		{name: "blockquote math", content: "> $$\n> [[fake]] #fake\n> $$\n> [[real]]", links: 1, tags: 0, linkTarget: "real"},
		{name: "list math", content: "- $$\n  [[fake]] #fake\n  $$\n- [[real]]", links: 1, tags: 0, linkTarget: "real"},
		{name: "malformed frontmatter", content: "---\ntags: [broken\n---\n$x #fake [[fake]]$\n[[real]]", links: 1, tags: 0, linkTarget: "real"},
	}

	p := NewParser()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := []byte(tt.content)
			before := append([]byte(nil), content...)
			got, err := p.Parse(content)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(content, before) {
				t.Fatal("parser changed source bytes")
			}
			if len(got.Links) != tt.links || len(got.Tags) != tt.tags {
				t.Fatalf("facts = %+v, want %d links and %d tags", got, tt.links, tt.tags)
			}
			if tt.linkTarget != "" && string(got.Links[0].RawTarget) != tt.linkTarget {
				t.Fatalf("link target = %q, want %q", got.Links[0].RawTarget, tt.linkTarget)
			}
		})
	}
}

func TestParserMathProtectionASTOwnership(t *testing.T) {
	p := NewParser()
	source := []byte("before [[real]] $x #fake [[fake]]$ after\n$$\n#block-fake [[block-fake]]\n$$\n[[after]]")
	root := p.plain.Parser().Parse(text.NewReader(source))

	var inline, block, links, tags int
	err := ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.(type) {
		case *MathInline:
			inline++
			if node.HasChildren() {
				t.Fatal("math inline must be a leaf")
			}
		case *MathBlock:
			block++
			if node.HasChildren() {
				t.Fatal("math block must be a leaf")
			}
		case *wikilink.Node:
			links++
		case *hashtag.Node:
			tags++
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if inline != 2 || block != 0 || links != 2 || tags != 0 {
		t.Fatalf("AST counts = inline %d, block %d, links %d, tags %d", inline, block, links, tags)
	}
}

func TestParserMathProtectionStandaloneDisplayASTOwnership(t *testing.T) {
	p := NewParser()
	source := []byte("$$\n#block-fake [[block-fake]]\n$$\n\n[[after]]")
	root := p.plain.Parser().Parse(text.NewReader(source))

	var inline, block, links, tags int
	err := ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.(type) {
		case *MathInline:
			inline++
		case *MathBlock:
			block++
			if node.HasChildren() {
				t.Fatal("standalone math block must be a leaf")
			}
		case *wikilink.Node:
			links++
		case *hashtag.Node:
			tags++
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if inline != 0 || block != 1 || links != 1 || tags != 0 {
		t.Fatalf("standalone AST counts = inline %d, block %d, links %d, tags %d", inline, block, links, tags)
	}
}

func TestParserMathProtectionRejectedCandidatesLeaveNormalAST(t *testing.T) {
	p := NewParser()
	source := []byte("$x [[real]]\n$$\nordinary [[second]]\n")
	root := p.plain.Parser().Parse(text.NewReader(source))
	var math, links int
	ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			switch node.(type) {
			case *MathInline, *MathBlock:
				math++
			case *wikilink.Node:
				links++
			}
		}
		return ast.WalkContinue, nil
	})
	if math != 0 || links != 2 {
		t.Fatalf("rejected candidate AST = math %d, links %d", math, links)
	}
}
