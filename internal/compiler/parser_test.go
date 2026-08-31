package compiler

import "testing"

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
