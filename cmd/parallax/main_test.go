package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func manifestFor(baseURL string, owned bool) string {
	o := "false"
	if owned {
		o = "true"
	}
	return `
apiVersion: parallax/v1
kind: Manifest
metadata: {name: e2e}
graph:
  nodes:
    - name: svc
      owned: ` + o + `
      http:
        baseURL: "` + baseURL + `"
        routes: [{method: GET, path: /healthz}, {method: DELETE, path: /all}]
`
}

func TestExitCodes(t *testing.T) {
	var deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
		}
		if r.Header.Get("traceparent") == "" {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	ok := writeManifest(t, manifestFor(srv.URL, true))
	if code := run([]string{"-manifest", ok, "-once", "-log-level", "error"}); code != 0 {
		t.Fatalf("answered pass: exit %d, want 0", code)
	}
	if deletes != 0 {
		t.Fatal("DELETE was sent; it is not repeat-safe")
	}
	if code := run([]string{"-manifest", ok, "-dry-run"}); code != 0 {
		t.Fatalf("dry run: exit %d, want 0", code)
	}

	srv.Close()
	if code := run([]string{"-manifest", ok, "-once", "-log-level", "error"}); code != 1 {
		t.Fatalf("unanswered pass: exit %d, want 1", code)
	}

	unowned := writeManifest(t, manifestFor("http://example.invalid", false))
	if code := run([]string{"-manifest", unowned, "-once"}); code != 2 {
		t.Fatalf("nothing owned: exit %d, want 2", code)
	}
	if code := run([]string{"-manifest", writeManifest(t, "apiVersion: nope\n")}); code != 2 {
		t.Fatalf("bad manifest: exit %d, want 2", code)
	}
	if code := run(nil); code != 2 {
		t.Fatalf("no manifest: exit %d, want 2", code)
	}
	if code := run([]string{"-manifest", ok, "-log-level", "loud"}); code != 2 {
		t.Fatalf("bad log level: exit %d, want 2", code)
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("PARALLAX_TEST_X", "")
	if envOr("PARALLAX_TEST_X", "d") != "d" {
		t.Fatal("empty env should fall back")
	}
	t.Setenv("PARALLAX_TEST_X", "v")
	if !strings.EqualFold(envOr("PARALLAX_TEST_X", "d"), "v") {
		t.Fatal("env should win")
	}
}

// The worker contract: manifest on stdin, one JSON object per line on stdout,
// nothing else on stdout.
func TestStdinManifestAndJSONLResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	m := `{"apiVersion":"parallax/v1","kind":"Manifest","metadata":{"name":"w"},` +
		`"graph":{"nodes":[{"name":"svc","owned":true,"http":{"baseURL":"` + srv.URL + `","routes":[{"method":"GET","path":"/healthz"}]}}]}}`

	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	go func() { _, _ = inW.Write([]byte(m)); _ = inW.Close() }()

	code := run([]string{"-manifest", "-", "-once", "-results", "jsonl", "-log-level", "error"})
	_ = outW.Close()
	os.Stdin, os.Stdout = oldIn, oldOut
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, _ := io.ReadAll(outR)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"type":"call"`) || !strings.Contains(lines[1], `"type":"pass"`) {
		t.Fatalf("stdout must be exactly a call line and a pass line, got:\n%s", b)
	}
	if code := run([]string{"-manifest", "x", "-results", "xml"}); code != 2 {
		t.Fatalf("bad -results: exit %d, want 2", code)
	}
}
