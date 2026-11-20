package repository

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

var png = "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 64)

func TestALogoIsFetchedFromPublicAddressesAlone(t *testing.T) {
	refused := []string{
		"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "fd00::1",
		"169.254.169.254", "fe80::1", "100.64.0.1", "0.0.0.0", "0.1.2.3", "::", "224.0.0.1", "ff02::1",
		"255.255.255.255", "198.18.0.1", "64:ff9b::a00:1", "2002:a00:1::", "::a00:1",
	}

	for _, a := range refused {
		if publicAddr(netip.MustParseAddr(a), false) {
			t.Errorf("%s is allowed", a)
		}

		if err := dialCheck(false)("tcp", netip.AddrPortFrom(netip.MustParseAddr(a), 443).String(), nil); !errors.Is(err, errAddress) {
			t.Errorf("the dialer took %s: %v", a, err)
		}
	}

	for _, a := range []string{"93.184.215.14", "2606:2800:21f:cb07:6820:80da:af6b:8b2c", "64:ff9b::5db8:d70e"} {
		if !publicAddr(netip.MustParseAddr(a), false) || dialCheck(false)("tcp", netip.AddrPortFrom(netip.MustParseAddr(a), 443).String(), nil) != nil {
			t.Errorf("%s is refused", a)
		}
	}

	// loopback is for tests alone, and nothing else private with it
	if !publicAddr(netip.MustParseAddr("127.0.0.1"), true) || publicAddr(netip.MustParseAddr("10.0.0.1"), true) || publicAddr(netip.MustParseAddr("169.254.169.254"), true) {
		t.Error("allowing loopback allows more")
	}
}

// logoServer serves a PNG, a redirect to it, one too big, and a page, and counts each request.
func logoServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		switch r.URL.Path {
		case "/logo.png":
			_, _ = w.Write([]byte(png)) //nolint:errcheck // a test server
		case "/moved.png":
			http.Redirect(w, r, "/logo.png", http.StatusFound)
		case "/big.png":
			w.Header().Set("Content-Type", "image/png")
			// no length up front: the cap holds as the body is read
			for range 300 {
				_, _ = w.Write([]byte(png + strings.Repeat("\x00", 1024-len(png)))) //nolint:errcheck // a test server
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		case "/page.png":
			_, _ = w.Write([]byte("<html><body>no image</body></html>")) //nolint:errcheck // a test server
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	return srv, &hits
}

func testOptions(t *testing.T, srv *httptest.Server) Options {
	t.Helper()

	tr, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("httptest's client has no *http.Transport")
	}

	return Options{AllowLocal: true, roots: tr.TLSClientConfig.RootCAs}
}

func TestALogosURLIsFetched(t *testing.T) {
	srv, _ := logoServer(t)
	opt := testOptions(t, srv)

	data, typ, err := fetchLogo(context.Background(), srv.URL+"/logo.png", "1.0.0", opt)
	if err != nil || typ != "image/png" || string(data) != png {
		t.Fatalf("%q %s %v", data, typ, err)
	}

	cases := map[string]struct {
		url  string
		opt  Options
		says string
	}{
		"a redirect":            {srv.URL + "/moved.png", opt, "redirects"},
		"a body over the cap":   {srv.URL + "/big.png", opt, "too big"},
		"no image":              {srv.URL + "/page.png", opt, "neither SVG, PNG, JPEG nor WebP"},
		"missing":               {srv.URL + "/none.png", opt, "404"},
		"loopback, untested":    {srv.URL + "/logo.png", Options{roots: opt.roots}, "never fetched from"},
		"localhost, untested":   {strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/logo.png", Options{roots: opt.roots}, "never fetched from"},
		"a private address":     {"https://10.0.0.1/logo.png", opt, "never fetched from"},
		"the metadata service":  {"https://169.254.169.254/latest/meta-data", opt, "never fetched from"},
		"plain http":            {"http://example.com/logo.png", opt, "an https URL"},
		"a user in the URL":     {"https://me@example.com/logo.png", opt, "an https URL"},
		"an untrusted local IP": {"https://[::ffff:10.0.0.1]/logo.png", opt, "never fetched from"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := fetchLogo(context.Background(), c.url, "1.0.0", c.opt)
			if !errors.Is(err, ErrLogo) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want %q", err, c.says)
			}
		})
	}
}

// List and AtRelease fetch a logo's URL, keep it by URL and version, and a logo that fails sets
// LogoErr and leaves the plugin listed.
func TestListingsFetchALogosURLOncePerVersion(t *testing.T) {
	srv, hits := logoServer(t)
	opt := testOptions(t, srv)
	opt.LogoCache = t.TempDir()

	withLogo := func(logo string) string {
		return strings.Replace(pluginManifest("lint", "driver", "Lints"), "logo: logo.svg", "logo: "+logo, 1)
	}

	r := newRepo(t)
	r.commit(map[string]string{"manifest.yaml": withLogo(srv.URL + "/logo.png")})
	r.git("tag", "v1.0.0")

	url := r.bare()

	list := func() Plugin {
		t.Helper()

		got, err := List(context.Background(), url, opt)
		if err != nil || len(got) != 1 || got[0].Err != nil {
			t.Fatalf("%+v %v", got, err)
		}

		return got[0]
	}

	if p := list(); p.LogoErr != nil || p.LogoType != "image/png" || string(p.LogoData) != png || p.Logo != srv.URL+"/logo.png" {
		t.Fatalf("%+v", p)
	}

	if p := list(); p.LogoErr != nil || hits.Load() != 1 {
		t.Fatalf("listed again: %v, %d fetches", p.LogoErr, hits.Load())
	}

	r.commit(map[string]string{"README": "x"})
	r.git("tag", "v1.1.0")
	url = r.bare()

	if p := list(); p.LogoErr != nil || hits.Load() != 2 {
		t.Fatalf("a new version: %v, %d fetches", p.LogoErr, hits.Load())
	}

	rel, err := AtRelease(context.Background(), url, ".", "1.0.0", opt)
	if err != nil || rel.LogoErr != nil || rel.LogoType != "image/png" || hits.Load() != 2 {
		t.Fatalf("at 1.0.0: %+v %v, %d fetches", rel, err, hits.Load())
	}

	r.commit(map[string]string{"manifest.yaml": withLogo(srv.URL + "/page.png")})
	r.git("tag", "v1.2.0")
	url = r.bare()

	if p := list(); !errors.Is(p.LogoErr, ErrLogo) || p.LogoData != nil {
		t.Fatalf("no image: %+v", p)
	}

	// a file logo is read at the version's commit
	r.commit(map[string]string{"manifest.yaml": pluginManifest("lint", "driver", "Lints"), "logo.svg": `<svg xmlns="http://www.w3.org/2000/svg"/>`})
	r.git("tag", "v1.3.0")
	url = r.bare()

	rel, err = AtRelease(context.Background(), url, ".", "v1.3.0", opt)
	if err != nil || rel.LogoErr != nil || rel.LogoType != "image/svg+xml" || rel.Logo != "logo.svg" {
		t.Fatalf("a file logo at 1.3.0: %+v %v", rel, err)
	}
}
