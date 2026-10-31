package docker

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Archive describes an image to be written in the format `docker save`
// produces since Docker 25: an OCI image layout (oci-layout, index.json,
// blobs/sha256/...) that also carries the legacy manifest.json. The classic
// image store reads manifest.json; the containerd image store reads
// index.json and keeps the original manifest digest.
type Archive struct {
	RepoTag           string // familiar name:tag, e.g. "nginx:latest"; empty for digest references
	Name              string // fully qualified reference, e.g. "docker.io/library/nginx:latest"
	RefName           string // tag only, e.g. "latest"
	ManifestDigest    string
	ManifestMediaType string
	RawManifest       []byte
	Config            string   // config digest
	Layers            []string // layer digests, in manifest order
	Open              func(digest string) (*os.File, error)
}

// CorruptBlobError reports a cached blob whose content no longer matches its
// digest.
type CorruptBlobError struct{ Digest string }

func (e *CorruptBlobError) Error() string {
	return fmt.Sprintf("cached blob %s is corrupt", e.Digest)
}

type dockerManifest struct {
	Config   string
	RepoTags []string
	Layers   []string
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

var epoch = time.Unix(0, 0)

func blobPath(d string) string { return "blobs/sha256/" + strings.TrimPrefix(d, "sha256:") }

// WriteArchive streams a as a tar archive. Every blob is re-hashed while it
// is written; a mismatch aborts with *CorruptBlobError before the archive is
// complete, so Docker rejects it as a whole.
func WriteArchive(w io.Writer, a Archive) error {
	tw := tar.NewWriter(w)
	for _, dir := range []string{"blobs/", "blobs/sha256/"} {
		if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: epoch}); err != nil {
			return err
		}
	}
	if err := writeFile(tw, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}

	written := map[string]bool{}
	for _, d := range append([]string{a.Config}, a.Layers...) {
		if written[d] {
			continue
		}
		written[d] = true
		if err := writeBlob(tw, d, a.Open); err != nil {
			return err
		}
	}
	if err := writeFile(tw, blobPath(a.ManifestDigest), a.RawManifest); err != nil {
		return err
	}

	desc := ociDescriptor{MediaType: a.ManifestMediaType, Digest: a.ManifestDigest, Size: int64(len(a.RawManifest))}
	if a.Name != "" {
		desc.Annotations = map[string]string{"io.containerd.image.name": a.Name}
		if a.RefName != "" {
			desc.Annotations["org.opencontainers.image.ref.name"] = a.RefName
		}
	}
	index, err := json.Marshal(ociIndex{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     []ociDescriptor{desc},
	})
	if err != nil {
		return err
	}
	if err := writeFile(tw, "index.json", index); err != nil {
		return err
	}

	m := dockerManifest{Config: blobPath(a.Config)}
	if a.RepoTag != "" {
		m.RepoTags = []string{a.RepoTag}
	}
	for _, l := range a.Layers {
		m.Layers = append(m.Layers, blobPath(l))
	}
	manifest, err := json.Marshal([]dockerManifest{m})
	if err != nil {
		return err
	}
	if err := writeFile(tw, "manifest.json", manifest); err != nil {
		return err
	}
	return tw.Close()
}

func writeFile(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data)), ModTime: epoch}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
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
	hdr := &tar.Header{Name: blobPath(d), Typeflag: tar.TypeReg, Mode: 0o644, Size: fi.Size(), ModTime: epoch}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(tw, io.TeeReader(f, h)); err != nil {
		return err
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != d {
		return &CorruptBlobError{Digest: d}
	}
	return nil
}
