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
			for range MaxFetch/1024 + 1 {
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

// fetchOne is the file at u, as Options.Fetch would give it.
func fetchOne(u string, opt Options) Fetched {
	return (&git{}).fetched(context.Background(), "", "1.0.0", []string{u}, opt)[u]
}

func TestAURLIsFetched(t *testing.T) {
	srv, _ := logoServer(t)
	opt := testOptions(t, srv)

	if f := fetchOne(srv.URL+"/logo.png", opt); f.Err != nil || f.Type != "image/png" || string(f.Data) != png {
		t.Fatalf("%+v", f)
	}

	if f := fetchOne(srv.URL+"/page.png", opt); f.Err != nil || !strings.HasPrefix(f.Type, "text/html") {
		t.Fatalf("a page: %+v", f)
	}

	cases := map[string]struct {
		url  string
		opt  Options
		says string
	}{
		"a redirect":            {srv.URL + "/moved.png", opt, "redirects"},
		"a body over the cap":   {srv.URL + "/big.png", opt, "too big"},
		"missing":               {srv.URL + "/none.png", opt, "404"},
		"loopback, untested":    {srv.URL + "/logo.png", Options{roots: opt.roots}, "never fetched from"},
		"localhost, untested":   {strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/logo.png", Options{roots: opt.roots}, "never fetched from"},
		"a private address":     {"https://10.0.0.1/logo.png", opt, "never fetched from"},
		"the metadata service":  {"https://169.254.169.254/latest/meta-data", opt, "never fetched from"},
		"plain http":            {"http://example.com/logo.png", opt, "nor an https URL"},
		"a user in the URL":     {"https://me@example.com/logo.png", opt, "nor an https URL"},
		"an untrusted local IP": {"https://[::ffff:10.0.0.1]/logo.png", opt, "never fetched from"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if f := fetchOne(c.url, c.opt); !errors.Is(f.Err, ErrFetch) || !strings.Contains(f.Err.Error(), c.says) {
				t.Fatalf("got %v, want %q", f.Err, c.says)
			}
		})
	}
}

// List and AtRelease fetch a URL Options.Fetch names, keep it by URL and version, and a fetch
// that fails sets its Err and leaves the plugin listed.
func TestListingsFetchAURLOncePerVersion(t *testing.T) {
	srv, hits := logoServer(t)
	opt := testOptions(t, srv)
	opt.FetchCache = t.TempDir()

	logo := srv.URL + "/logo.png"
	opt.Fetch = func(Plugin) []string { return []string{logo} }

	r := newRepo(t)
	r.commit(map[string]string{"manifest.yaml": pluginManifest("lint", "driver", "Lints")})
	r.git("tag", "v1.0.0")

	url := r.bare()

	list := func() Fetched {
		t.Helper()

		got, err := List(context.Background(), url, opt)
		if err != nil || len(got) != 1 || got[0].Err != nil {
			t.Fatalf("%+v %v", got, err)
		}

		return got[0].Fetched[logo]
	}

	if f := list(); f.Err != nil || f.Type != "image/png" || string(f.Data) != png {
		t.Fatalf("%+v", f)
	}

	if f := list(); f.Err != nil || hits.Load() != 1 {
		t.Fatalf("listed again: %v, %d fetches", f.Err, hits.Load())
	}

	r.commit(map[string]string{"README": "x"})
	r.git("tag", "v1.1.0")
	url = r.bare()

	if f := list(); f.Err != nil || hits.Load() != 2 {
		t.Fatalf("a new version: %v, %d fetches", f.Err, hits.Load())
	}

	rel, err := AtRelease(context.Background(), url, ".", "1.0.0", opt)
	if err != nil || rel.Fetched[logo].Err != nil || rel.Fetched[logo].Type != "image/png" || hits.Load() != 2 {
		t.Fatalf("at 1.0.0: %+v %v, %d fetches", rel.Fetched, err, hits.Load())
	}

	logo = srv.URL + "/none.png"

	if f := list(); !errors.Is(f.Err, ErrFetch) || f.Data != nil {
		t.Fatalf("missing: %+v", f)
	}
}
