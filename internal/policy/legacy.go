package policy

import (
	"strings"
)

// LegacyRule is a structured exclude rule in its raw stored form.
type LegacyRule struct {
	Kind  string // exclude_path_prefix | exclude_dir_name | exclude_filename | exclude_extension
	Value string
}

// ConvertLegacyRules builds a synthetic root-scoped Gitignore snapshot from the
// structured excludes (§6.2). It preserves the legacy engine's actual behavior,
// escaping Git metacharacters so historically-literal values remain literal. A
// profile with no rules produces a valid empty snapshot.
func ConvertLegacyRules(rules []LegacyRule) *IgnoreSnapshot {
	var lines []string
	for _, r := range rules {
		lines = append(lines, convertLegacyRule(r))
	}
	if len(lines) == 0 {
		return &IgnoreSnapshot{}
	}
	content := []byte(strings.Join(lines, "\n"))
	content = append(content, '\n')
	snap := &IgnoreSnapshot{
		Files: []File{
			{RelativePath: LegacySourceName, ScopeDir: "", Content: content},
		},
	}
	snap.Warnings = MatcherWarnings(snap.Matcher())
	return snap
}

// convertLegacyRule translates one structured rule into a Gitignore pattern.
//
//   - exclude_path_prefix becomes a root-anchored literal path pattern that
//     excludes that path and descendants (/path).
//   - exclude_dir_name becomes a Gitignore expression that excludes descendants
//     under directories with that literal name at arbitrary depth (name/).
//   - exclude_filename becomes an unanchored literal name pattern.
//   - exclude_extension preserves case-insensitive extension comparison using a
//     bracket expression ([eE][xX][tT]).
//
// Every metacharacter in the value is escaped so the value stays literal.
func convertLegacyRule(r LegacyRule) string {
	switch r.Kind {
	case "exclude_path_prefix":
		// Root-anchored: excludes the exact path and descendants.
		v := strings.TrimPrefix(strings.TrimSpace(r.Value), "/")
		if v == "" {
			return ""
		}
		return "/" + escapeGlob(v)
	case "exclude_dir_name":
		v := strings.TrimSpace(r.Value)
		if strings.Contains(v, "/") {
			// A nested dir-name path prefix is root-anchored.
			v = strings.TrimPrefix(v, "/")
			if v == "" {
				return ""
			}
			return "/" + escapeGlob(v) + "/"
		}
		if v == "" {
			return ""
		}
		// Unanchored dir name: matches a directory with that name at any depth.
		return "**/" + escapeGlob(v) + "/"
	case "exclude_filename":
		v := strings.TrimSpace(r.Value)
		if v == "" {
			return ""
		}
		return escapeGlob(v)
	case "exclude_extension":
		v := strings.TrimPrefix(strings.TrimSpace(r.Value), ".")
		if v == "" {
			return ""
		}
		// Case-insensitive extension via bracket expression. The legacy engine
		// lowercased both sides, so uppercase extensions matched too.
		return "*." + caseFoldExt(v)
	}
	return ""
}

// escapeGlob escapes Git glob metacharacters so the value remains literal.
func escapeGlob(v string) string {
	var sb strings.Builder
	for _, r := range v {
		switch r {
		case '*', '?', '[', ']', '!', '#', '\\':
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// caseFoldExt renders an extension as a case-insensitive bracket expression,
// e.g. "tmp" -> "[tT][mM][pP]".
func caseFoldExt(ext string) string {
	var sb strings.Builder
	for _, r := range ext {
		if r >= 'a' && r <= 'z' {
			sb.WriteByte('[')
			sb.WriteRune(r)
			sb.WriteRune(r - 'a' + 'A')
			sb.WriteByte(']')
		} else if r >= 'A' && r <= 'Z' {
			sb.WriteByte('[')
			sb.WriteRune(r - 'A' + 'a')
			sb.WriteRune(r)
			sb.WriteByte(']')
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
