package main

import "testing"

func TestIsFilesystemPath(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		// Real paths.
		{"/opt/idp/data/import/service-realm.json", true},
		{"/etc/ssl/certs", true},
		{"data/seed/fixtures.sql", true},
		{"./config/app.yaml", true},
		{"../migrations/001.up.sql", true},
		{`C:\Users\svc\config.json`, true},
		{"~/.config/svc.yaml", true},

		// HTTP routes and gRPC methods, which read like absolute paths but
		// address a server, not a disk.
		{"/healthz", false},
		{"/api/v1/widgets", false},
		{"/svc.v1.WidgetService/ListWidgets", false},

		// A MIME type is `type/subtype`, and Office subtypes carry dots.
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", false},
		{"text/csv", false},

		// SQL fragments -- what a looser implementation mistakes for paths, because of
		// the escaped backslash in the ESCAPE clause.
		{` ILIKE ? ESCAPE '\'`, false},
		{"AND (u.first_name ILIKE ? ESCAPE ", false},

		// URLs are reported under their own rule.
		{"https://example.com/a/b.json", false},

		{"", false},
		{"plain", false},
	}
	for _, tc := range tests {
		if got := isFilesystemPath(tc.in); got != tc.want {
			t.Errorf("isFilesystemPath(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestFindHardcodedLiterals(t *testing.T) {
	idx := load(t, map[string]string{
		"a/a.go": `package a

import "example.com/m/b"

type T struct {
	Name string ` + "`json:\"name\" db:\"https://not-a-url\"`" + `
}

const seed = "data/seed/fixtures.sql"

func Handler() string {
	_ = b.X
	route := "/api/v1/widgets"
	_ = route
	// An allowlisted reference alongside the real finding, so the rule is
	// shown to separate them rather than to report or drop both.
	_ = "https://www.rfc-editor.org/rfc/rfc9457"
	return "https://idp.internal.example-corp.nl/realms/svc"
}
`,
		"b/b.go": "package b\n\nvar X = 1\n",
	}, DefaultConfig())

	urls := idx.findHardcodedURLs()
	if len(urls) != 1 {
		t.Fatalf("got %d URL findings, want 1 (struct tags, import paths and allowlisted hosts must be skipped): %+v", len(urls), urls)
	}
	if want := "https://idp.internal.example-corp.nl/realms/svc"; urls[0].Value != want {
		t.Errorf("url value = %q, want %q", urls[0].Value, want)
	}
	if want := "Handler"; urls[0].Element != want {
		t.Errorf("url element = %q, want %q", urls[0].Element, want)
	}

	paths := idx.findHardcodedPaths()
	if len(paths) != 1 {
		t.Fatalf("got %d path findings, want 1 (the route is not a path): %+v", len(paths), paths)
	}
	if want := "data/seed/fixtures.sql"; paths[0].Value != want {
		t.Errorf("path value = %q, want %q", paths[0].Value, want)
	}
	if want := "a.go"; paths[0].Element != want {
		t.Errorf("path element = %q, want %q (a package-level const has no enclosing function)", paths[0].Element, want)
	}
}

// A literal can be a whole HTML template. One that opens with an allowlisted
// namespace must still report a real endpoint further down it, or the
// allowlist becomes a way to hide findings behind an xmlns.
func TestFindHardcodedURLs_AllowlistedPrefixDoesNotMaskEndpoint(t *testing.T) {
	idx := load(t, map[string]string{
		"a/a.go": `package a

const tmpl = "<html xmlns=\"http://www.w3.org/1999/xhtml\">" +
	"<a href=\"https://idp.corp-two.nl/realms/svc\">go</a></html>"

const benignOnly = "<html xmlns=\"http://www.w3.org/1999/xhtml\"></html>"
`,
	}, DefaultConfig())

	urls := idx.findHardcodedURLs()
	if len(urls) != 1 {
		t.Fatalf("got %d URL findings, want 1: %+v", len(urls), urls)
	}
	if want := "https://idp.corp-two.nl/realms/svc"; urls[0].Value != want {
		t.Errorf("url value = %q, want %q (the namespace must be skipped, not the literal)", urls[0].Value, want)
	}
}

func TestURLHost(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://www.rfc-editor.org/rfc/rfc9457", "www.rfc-editor.org"},
		{"http://www.w3.org/1999/xhtml", "www.w3.org"},
		// A format string: the `%s` is why net/url cannot be used here.
		{"https://blob.test/%s?expires=%d", "blob.test"},
		{"https://API.Example.COM:8443/v1", "api.example.com"},
		{"https://user:pw@internal.corp.nl/path", "internal.corp.nl"},
		{"http://[::1]:9000/minio", "[::1]"},
		{"https://plain.corp.nl", "plain.corp.nl"},
		{"https://frag.corp.nl#anchor", "frag.corp.nl"},
	}
	for _, tc := range tests {
		if got := urlHost(tc.in); got != tc.want {
			t.Errorf("urlHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestURLHostAllowed(t *testing.T) {
	cfg := DefaultConfig()
	tests := []struct {
		host string
		want bool
		why  string
	}{
		// Namespace identifiers and specification references.
		{"www.w3.org", true, "XHTML namespace host"},
		{"www.rfc-editor.org", true, "spec reference host"},
		{"datatracker.ietf.org", true, "spec reference host"},
		// Names RFC 2606 and RFC 6761 reserve for fixtures.
		{"blob.test", true, "reserved .test TLD"},
		{"example.com", true, "reserved second-level name"},
		{"www.example.org", true, "subdomain of a reserved name"},
		{"api.localhost", true, "reserved .localhost TLD"},
		// Real addresses, which must still be reported.
		{"localhost", false, "a hardcoded local service is worth reporting"},
		{"test", false, "a reserved TLD is exempt as a parent domain, not as a host"},
		{"notexample.com", false, "allowlist must match on label boundaries"},
		{"w3.org.attacker.nl", false, "allowlist must anchor at the end"},
		{"idp.corp-two.nl", false, "a real endpoint"},
		{"", false, "an unparseable authority is not allowlisted"},
	}
	for _, tc := range tests {
		if got := cfg.urlHostAllowed(tc.host); got != tc.want {
			t.Errorf("urlHostAllowed(%q) = %v, want %v (%s)", tc.host, got, tc.want, tc.why)
		}
	}

	// A config entry extends the defaults and accepts a leading dot.
	cfg.URLAllowlistHosts = append(cfg.URLAllowlistHosts, ".Schemas.Corp.NL")
	if !cfg.urlHostAllowed("ns.schemas.corp.nl") {
		t.Error("a configured host was not allowlisted (entry should be case- and dot-insensitive)")
	}
	if cfg.urlHostAllowed("idp.corp-two.nl") {
		t.Error("adding a config entry must not allowlist unrelated hosts")
	}
}

func TestBandRateAndDescribe(t *testing.T) {
	b := Band{High: 200, VeryHigh: 500}
	tests := []struct {
		v       int
		sev     Severity
		explain string
	}{
		{1, SeverityNone, ""},
		{200, SeverityNone, ""},
		{201, SeverityHigh, "(high risk, [201 - 500])"},
		{500, SeverityHigh, "(high risk, [201 - 500])"},
		{501, SeverityVeryHigh, "(very high risk, [> 500])"},
	}
	for _, tc := range tests {
		if got := b.Rate(tc.v); got != tc.sev {
			t.Errorf("Rate(%d) = %v, want %v", tc.v, got, tc.sev)
		}
		if got := b.Describe(tc.v); got != tc.explain {
			t.Errorf("Describe(%d) = %q, want %q", tc.v, got, tc.explain)
		}
	}
}
