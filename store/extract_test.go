package store

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// git pads an archive to a whole record after the tar's end; a pipe that has no room for the
// padding leaves git blocked on its write. An unbuffered pipe is the smallest pipe there is: the
// writer ends only if the reader reads the padding too.
func TestTheArchiveIsReadToItsEndPastThePadding(t *testing.T) {
	var archive bytes.Buffer

	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.yaml", Mode: 0o644, Size: 5}); err != nil {
		t.Fatal(err)
	}

	if _, err := tw.Write([]byte("kind\n")); err != nil {
		t.Fatal(err)
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	r, w := io.Pipe()
	ended := make(chan error, 1)

	go func() {
		_, err := w.Write(append(archive.Bytes(), make([]byte, 1<<20)...))
		ended <- err

		_ = w.Close() //nolint:errcheck // the pipe has no error to give on close
	}()

	dir := t.TempDir()
	if err := extract(r, dir); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("writer: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = r.Close() //nolint:errcheck // only to free the writer

		t.Fatal("the writer is still blocked on the padding after the tar's end")
	}

	data, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil || string(data) != "kind\n" {
		t.Fatalf("manifest.yaml %q, %v", data, err)
	}
}
