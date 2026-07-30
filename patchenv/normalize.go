// Package patchenv normalizes Codex apply_patch freeform envelopes so that
// common model mistakes (especially trailing stars on Begin/End lines) do not
// fail client-side verification.
package patchenv

import "strings"

// Normalize rewrites apply_patch envelope lines that models often emit with
// trailing stars (Markdown-style), which Codex rejects:
//
//	invalid patch: The first line of the patch must be '*** Begin Patch'
//
// Only full-line Begin/End markers are touched; file content is left alone.
func Normalize(s string) string {
	if s == "" || !strings.Contains(s, "Patch") {
		return s
	}
	// Fast path for the exact mistake seen in Grok rollouts.
	s = NormalizeFragments(s)

	// Line-oriented pass: tolerate extra spaces / trailing junk on envelope lines.
	// Keep original line endings (\n vs \r\n).
	var b strings.Builder
	b.Grow(len(s))
	start := 0
	for start <= len(s) {
		nl := strings.IndexByte(s[start:], '\n')
		var line string
		var sep string
		if nl < 0 {
			line = s[start:]
			sep = ""
			start = len(s) + 1
		} else {
			line = s[start : start+nl]
			sep = "\n"
			start = start + nl + 1
		}
		cr := strings.HasSuffix(line, "\r")
		core := line
		if cr {
			core = line[:len(line)-1]
		}
		trimmed := strings.TrimSpace(core)
		switch {
		case isLooseEnvelopeLine(trimmed, "Begin Patch"):
			core = "*** Begin Patch"
		case isLooseEnvelopeLine(trimmed, "End Patch"):
			core = "*** End Patch"
		}
		if cr {
			core += "\r"
		}
		b.WriteString(core)
		b.WriteString(sep)
		if nl < 0 {
			break
		}
	}
	return b.String()
}

// NormalizeFragments replaces the exact bad tokens Grok commonly emits,
// including mid-stream delta chunks that contain a full marker.
func NormalizeFragments(s string) string {
	if s == "" {
		return s
	}
	repls := []struct{ old, new string }{
		{"*** Begin Patch ***", "*** Begin Patch"},
		{"*** End Patch ***", "*** End Patch"},
		{"***Begin Patch***", "*** Begin Patch"},
		{"***End Patch***", "*** End Patch"},
		{"*** Begin Patch***", "*** Begin Patch"},
		{"*** End Patch***", "*** End Patch"},
		{"***Begin Patch ***", "*** Begin Patch"},
		{"***End Patch ***", "*** End Patch"},
	}
	for _, r := range repls {
		if strings.Contains(s, r.old) {
			s = strings.ReplaceAll(s, r.old, r.new)
		}
	}
	return s
}

func isLooseEnvelopeLine(trimmed, kind string) bool {
	// kind is "Begin Patch" or "End Patch"
	if trimmed == "*** "+kind {
		return true // already correct; rewrite is a no-op
	}
	if !strings.HasPrefix(trimmed, "***") {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "***"))
	if !strings.HasPrefix(strings.ToLower(rest), strings.ToLower(kind)) {
		return false
	}
	after := strings.TrimSpace(rest[len(kind):])
	for _, r := range after {
		if r != '*' && r != ' ' && r != '\t' {
			return false
		}
	}
	return true
}
