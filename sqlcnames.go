package main

// sqlc's Go naming, reproduced exactly.
//
// The plugin emits code that must COMPILE beside sqlc's own output: a wrapper
// returning []Note is a build error unless sqlc also decided the `notes` table
// models as `Note`. So these two functions are a faithful port of
// internal/codegen/golang.StructName and internal/inflection.Singular at sqlc
// v1.31.1 (which wraps github.com/jinzhu/inflection v1.0.0), not an
// approximation — an approximation would be a broken product, quietly, for
// whichever table happened to pluralize irregularly.
//
// Only the lowercase path is ported: every identifier reaching here has
// already passed identRe, and jinzhu's uppercase/title rule variants can only
// match input this side never sees.

import (
	"regexp"
	"strings"
	"unicode"
)

// goName is sqlc's StructName: split on non-alphanumerics, title-case each
// part, and shout the default initialism list (which is exactly {"id"}).
func goName(s string) string {
	var out strings.Builder
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if part == "id" {
			out.WriteString("ID")
			continue
		}
		// strings.Title's rule, which sqlc still relies on: a letter that
		// follows a non-letter is a word start, so "v2col" becomes "V2Col".
		prev := ' '
		for _, r := range part {
			if !unicode.IsLetter(prev) {
				out.WriteRune(unicode.ToUpper(r))
			} else {
				out.WriteRune(r)
			}
			prev = r
		}
	}
	if out.Len() > 0 && unicode.IsDigit(rune(out.String()[0])) {
		return "_" + out.String()
	}
	return out.String()
}

// modelName is the Go type sqlc gives one table's row: the singular, titled.
func modelName(table string) string { return goName(singular(table)) }

// singularRule is one find/replace of the inflection table.
type singularRule struct {
	re   *regexp.Regexp
	repl string
}

// singularRules is jinzhu/inflection's compiled singular map for lowercase
// input: uncountables, then irregular plurals, then the regular rules in
// REVERSE declaration order — first match wins, exactly as upstream.
var singularRules = func() []singularRule {
	var rules []singularRule
	add := func(find, repl string) {
		rules = append(rules, singularRule{regexp.MustCompile(find), repl})
	}
	for _, u := range []string{"equipment", "information", "rice", "money",
		"species", "series", "fish", "sheep", "jeans", "police"} {
		add("^(?i)("+u+")$", "${1}")
	}
	for _, ir := range [][2]string{{"people", "person"}, {"men", "man"},
		{"children", "child"}, {"sexes", "sex"}, {"moves", "move"},
		{"mombies", "mombie"}} {
		add(ir[0]+"$", ir[1])
	}
	// Upstream's singularInflections, reversed.
	for _, r := range [][2]string{
		{"(database)s$", "${1}"},
		{"(quiz)zes$", "${1}"},
		{"(matr)ices$", "${1}ix"},
		{"(vert|ind)ices$", "${1}ex"},
		{"^(ox)en", "${1}"},
		{"(alias|status)(es)?$", "${1}"},
		{"(octop|vir)(us|i)$", "${1}us"},
		{"^(a)x[ie]s$", "${1}xis"},
		{"(cris|test)(is|es)$", "${1}is"},
		{"(shoe)s$", "${1}"},
		{"(o)es$", "${1}"},
		{"(bus)(es)?$", "${1}"},
		{"^(m|l)ice$", "${1}ouse"},
		{"(x|ch|ss|sh)es$", "${1}"},
		{"(c)ookies$", "${1}ookie"},
		{"(m)ovies$", "${1}ovie"},
		{"(s)eries$", "${1}eries"},
		{"([^aeiouy]|qu)ies$", "${1}y"},
		{"([lr])ves$", "${1}f"},
		{"(tive)s$", "${1}"},
		{"(hive)s$", "${1}"},
		{"([^f])ves$", "${1}fe"},
		{"(^analy)(sis|ses)$", "${1}sis"},
		{"((a)naly|(b)a|(d)iagno|(p)arenthe|(p)rogno|(s)ynop|(t)he)(sis|ses)$", "${1}sis"},
		{"([ti])a$", "${1}um"},
		{"(n)ews$", "${1}ews"},
		{"(ss)$", "${1}"},
		{"s$", ""},
	} {
		add(r[0], r[1])
	}
	return rules
}()

// sqlcSingularFixes are the words sqlc corrects before delegating to jinzhu.
var sqlcSingularFixes = map[string]string{
	"campus": "campus", "meta": "meta", "calories": "calorie",
	"waves": "wave", "metadata": "metadata",
}

func singular(s string) string {
	if fixed, ok := sqlcSingularFixes[strings.ToLower(s)]; ok {
		return fixed
	}
	for _, r := range singularRules {
		if r.re.MatchString(s) {
			return r.re.ReplaceAllString(s, r.repl)
		}
	}
	return s
}
