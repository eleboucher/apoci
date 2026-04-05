package oci

import (
	"io"

	"cuelabs.dev/go/oci/ociregistry"
)

type blobReader struct {
	rc   io.ReadCloser
	desc ociregistry.Descriptor
}

func newBlobReader(rc io.ReadCloser, desc ociregistry.Descriptor) ociregistry.BlobReader {
	return &blobReader{rc: rc, desc: desc}
}

func (r *blobReader) Read(p []byte) (int, error)         { return r.rc.Read(p) }
func (r *blobReader) Close() error                       { return r.rc.Close() }
func (r *blobReader) Descriptor() ociregistry.Descriptor { return r.desc }
