package project

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ignoredBelow reports whether a directory on the path from the work tree
// root down to dir (exclusive of root) is ignored by the repository: then
// dir is not part of that repository's project, even if some files under it
// were force-added. Patterns come from .git/info/exclude and the .gitignore
// files of root and of each directory on the path, with git's precedence
// (deeper files win, last match wins) and its rule that nothing below an
// ignored directory is considered.
func ignoredBelow(root, commonDir, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")

	rules := readIgnore(filepath.Join(commonDir, "info", "exclude"), "")
	rules = append(rules, readIgnore(filepath.Join(root, ".gitignore"), "")...)
	for i := range parts {
		path := strings.Join(parts[:i+1], "/")
		ignored := false
		for _, r := range rules {
			if r.match(path) {
				ignored = !r.negate
			}
		}
		if ignored {
			return true
		}
		rules = append(rules, readIgnore(filepath.Join(root, filepath.FromSlash(path), ".gitignore"), path)...)
	}
	return false
}

type ignoreRule struct {
	re     *regexp.Regexp
	negate bool
}

// match reports whether the directory path (relative to the work tree root,
// slash-separated) matches.
func (r ignoreRule) match(path string) bool { return r.re.MatchString(path) }

// readIgnore parses a gitignore file located in directory base (relative to
// the work tree root, "" for the root). Missing or unreadable files have no
// rules.
func readIgnore(file, base string) []ignoreRule {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rules []ignoreRule
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if r, ok := parseIgnore(sc.Text(), base); ok {
			rules = append(rules, r)
		}
	}
	return rules
}

// parseIgnore compiles one gitignore line. Only directories are matched, so
// a trailing slash needs no special handling.
func parseIgnore(line, base string) (ignoreRule, bool) {
	line = strings.TrimRight(line, " \t\r")
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	var r ignoreRule
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\`) {
		line = line[1:] // \# and \! are literal
	}
	line = strings.TrimSuffix(line, "/")
	if line == "" {
		return ignoreRule{}, false
	}
	// A slash at the start or in the middle anchors the pattern to base;
	// otherwise it matches a name at any depth below base.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")

	prefix := ""
	if base != "" {
		prefix = regexp.QuoteMeta(base) + "/"
	}
	body := globRegexp(line)
	expr := "^" + prefix + body + "$"
	if !anchored {
		expr = "^" + prefix + "(?:.*/)?" + body + "$"
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return ignoreRule{}, false
	}
	r.re = re
	return r, true
}

// globRegexp translates a gitignore glob: * and ? stay within a path
// segment, ** spans segments, [...] is a character class.
func globRegexp(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "/**") && i+3 == len(glob):
			b.WriteString("/.*")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			end := strings.IndexByte(glob[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := glob[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		case c == '\\' && i+1 < len(glob):
			i++
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}
