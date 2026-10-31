package modelrunner

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Hkarimi561/ddm/internal/docker"
)

// Archive is a model to send to the runner's /models/load.
type Archive struct {
	RawManifest []byte
	Config      string   // config digest
	Layers      []string // layer digests, in manifest order
	Open        func(digest string) (*os.File, error)
}

var epoch = time.Unix(0, 0)

// WriteModelTar writes a in the layout the runner's tarball reader expects
// (the same one `docker model package` produces):
//
//	blobs/  blobs/sha256/  blobs/sha256/<layer>...  blobs/sha256/<config>  manifest.json
//
// Model layers are uncompressed, so a layer's digest is also its diff ID,
// which is what the runner names blobs by. Each blob is re-hashed while it
// is written; a mismatch aborts with *docker.CorruptBlobError before the
// manifest is sent, so the runner never registers the model.
func WriteModelTar(w io.Writer, a Archive) error {
	tw := tar.NewWriter(w)
	for _, dir := range []string{"blobs", "blobs/sha256"} {
		if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: epoch}); err != nil {
			return err
		}
	}
	written := map[string]bool{}
	for _, d := range append(append([]string(nil), a.Layers...), a.Config) {
		if written[d] {
			continue
		}
		written[d] = true
		if err := writeBlob(tw, d, a.Open); err != nil {
			return err
		}
	}
	hdr := &tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(a.RawManifest)), ModTime: epoch}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := tw.Write(a.RawManifest); err != nil {
		return err
	}
	return tw.Close()
}

func writeBlob(tw *tar.Writer, d string, open func(string) (*os.File, error)) error {
	f, err := open(d)
	if err != nil {
		return fmt.Errorf("opening cached blob %s: %w", d, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	hdr := &tar.Header{Name: "blobs/sha256/" + strings.TrimPrefix(d, "sha256:"), Typeflag: tar.TypeReg, Mode: 0o644, Size: fi.Size(), ModTime: epoch}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(tw, io.TeeReader(f, h)); err != nil {
		return err
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != d {
		return &docker.CorruptBlobError{Digest: d}
	}
	return nil
}
