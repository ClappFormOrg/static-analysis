package main

import (
	"go/ast"
	"go/token"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Literal is one hardcoded string literal a rule objected to.
type Literal struct {
	Element string
	File    *File
	Line    int
	Value   string
}

var (
	// A scheme followed by an authority. Deliberately not anchored: the
	// a commercial scan's own hits include URLs embedded in HTML fragments and prose,
	// not just literals that are entirely a URL.
	urlPattern = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"'<>)\\]+`)

	// A Windows path, or a `~`-relative one.
	winPathPattern  = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	homePathPattern = regexp.MustCompile(`^~[\\/]`)

	// A relative path with at least one separator, ending in a file
	// extension: `seed/demo.sql`, `./config.yaml`, `../migrations/001.up.sql`.
	relPathPattern = regexp.MustCompile(`^\.{0,2}/?[\w.\-]+(/[\w.\-]+)+\.[A-Za-z][A-Za-z0-9]{0,7}$`)

	// A MIME type is `type/subtype`, which reads exactly like a relative path
	// once the subtype carries a dot -- as
	// `application/vnd.openxmlformats-officedocument.spreadsheetml.sheet` does.
	mimePattern = regexp.MustCompile(`^(application|audio|font|image|message|model|multipart|text|video)/`)
)

// filesystemRoots are the absolute prefixes that mean a literal is addressing a
// real filesystem rather than an HTTP route. Without this list every `/healthz`
// and `/v1/widgets` in a server codebase reads as a hardcoded path, which is how
// a looser implementation ends up reporting SQL fragments and no actual paths.
//
// The names are held bare and slashed by rootPrefixes rather than written out
// as "/etc/" and friends, because a table of absolute-path literals IS a table
// of hardcoded paths: spelled the obvious way, this rule reported its own
// vocabulary, once per entry, and the only answers were to suppress the rule
// against itself or to say what the entries actually are. They are directory
// names; the leading and trailing slash is the prefix test's business, not
// theirs.
var filesystemRoots = rootPrefixes(
	"etc", "var", "tmp", "usr", "opt", "home", "root",
	"proc", "sys", "dev", "mnt", "srv", "app", "run",
)

// rootPrefixes renders each top-level directory name as the `/name/` form
// isHardcodedPath matches on.
func rootPrefixes(dirs ...string) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = "/" + d + "/"
	}
	return out
}

// reservedTLDs are the top-level domains RFC 2606 and RFC 6761 set aside so
// examples and test fixtures have names guaranteed never to resolve. A literal
// addressing one cannot be a production endpoint by construction, so it is not
// a hardcoded service address however much it looks like one.
//
// These match as a parent domain only, never as the whole host: `blob.test` is
// a fixture, but a bare single-label `localhost` is a real local service, and
// hardcoding one is exactly what this rule should report.
var reservedTLDs = []string{"test", "example", "invalid", "localhost"}

// reservedDomains are the second-level names RFC 2606 reserves for the same
// purpose. Unlike the TLDs these are usable as the host itself.
var reservedDomains = []string{"example.com", "example.net", "example.org"}

// findHardcodedURLs reports string literals containing an absolute URL, minus
// the ones whose host is an identifier or a reference rather than an endpoint.
//
// Every URL in the literal is considered, not just the first. A literal here
// can be a whole HTML template, and one that opens with an XML namespace must
// not hide a real endpoint further down it.
func (idx *Index) findHardcodedURLs() []Literal {
	return idx.scanLiterals(func(s string) (string, bool) {
		for _, m := range urlPattern.FindAllString(s, -1) {
			if !idx.Cfg.urlHostAllowed(urlHost(m)) {
				return m, true
			}
		}
		return "", false
	})
}

// urlHostAllowed reports whether a URL host is one the scan does not treat as
// a service address: a reserved name, or one the config named explicitly.
func (c Config) urlHostAllowed(host string) bool {
	if host == "" {
		return false
	}
	for _, tld := range reservedTLDs {
		if strings.HasSuffix(host, "."+tld) {
			return true
		}
	}
	for _, name := range reservedDomains {
		if hostMatches(host, name) {
			return true
		}
	}
	for _, name := range c.URLAllowlistHosts {
		if hostMatches(host, strings.ToLower(strings.TrimPrefix(name, "."))) {
			return true
		}
	}
	return false
}

// hostMatches matches a host against an allowlist name on label boundaries, so
// `example.com` covers `www.example.com` but not `notexample.com`.
func hostMatches(host, name string) bool {
	return name != "" && (host == name || strings.HasSuffix(host, "."+name))
}

// urlHost returns the lowercased host of a matched URL, without userinfo or
// port. net/url is deliberately not used: these are source literals, and one
// of them is a format string whose `%s` the URL parser rejects as a bad escape.
func urlHost(raw string) string {
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	// Strip the port. A colon inside the brackets of an IPv6 literal is part
	// of the address, so only a colon after the closing bracket separates one.
	if i := strings.LastIndex(rest, "]"); i >= 0 {
		rest = rest[:i+1]
	} else if i := strings.LastIndex(rest, ":"); i >= 0 {
		rest = rest[:i]
	}
	return strings.ToLower(rest)
}

// findHardcodedPaths reports string literals that address the filesystem.
func (idx *Index) findHardcodedPaths() []Literal {
	return idx.scanLiterals(func(s string) (string, bool) {
		if isFilesystemPath(s) {
			return s, true
		}
		return "", false
	})
}

func isFilesystemPath(s string) bool {
	s = strings.TrimSpace(s)
	// Anything with whitespace, query syntax, template markup or format verbs
	// spanning the whole value is prose or SQL, not a path.
	if s == "" || len(s) > 200 || strings.ContainsAny(s, " \t\n\"'?<>{}|") {
		return false
	}
	if urlPattern.MatchString(s) {
		return false // reported as a URL instead
	}
	if mimePattern.MatchString(s) {
		return false
	}
	switch {
	case winPathPattern.MatchString(s), homePathPattern.MatchString(s):
		return true
	}
	if strings.HasPrefix(s, "/") {
		for _, root := range filesystemRoots {
			if strings.HasPrefix(s, root) {
				return true
			}
		}
		// An absolute path is only a path if its last segment has a file
		// extension; otherwise it is an HTTP route or a gRPC method name.
		return strings.Count(s, "/") >= 2 && path.Ext(s) != ""
	}
	return relPathPattern.MatchString(s)
}

// scanLiterals walks every string literal in the module and hands its decoded
// value to match. Import paths and struct tags are skipped: neither is a value
// anyone hardcoded by choice.
func (idx *Index) scanLiterals(match func(string) (string, bool)) []Literal {
	var out []Literal
	for _, f := range idx.Files {
		fileElement := path.Base(f.Rel)
		for _, d := range f.AST.Decls {
			element := fileElement
			if fd, ok := d.(*ast.FuncDecl); ok {
				element = idx.element(fd)
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.ImportSpec:
					return false
				case *ast.Field:
					// A struct tag is a string literal that is really syntax.
					// Field types carry no literals worth reporting, so the
					// whole field is skipped rather than partially walked.
					return false
				case *ast.BasicLit:
					if n.Kind != token.STRING {
						return false
					}
					v, err := strconv.Unquote(n.Value)
					if err != nil {
						return false
					}
					if m, ok := match(v); ok {
						out = append(out, Literal{
							Element: element,
							File:    f,
							Line:    idx.Fset.Position(n.Pos()).Line,
							Value:   m,
						})
					}
					return false
				}
				return true
			})
		}
	}
	return out
}
