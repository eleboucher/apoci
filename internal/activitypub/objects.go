package activitypub

import (
	"encoding/base64"
	"time"
)

// OCI-specific ActivityPub object types, extending the AS2 vocabulary.

const (
	ContextActivityStreams = "https://www.w3.org/ns/activitystreams"
	ContextSecurity        = "https://w3id.org/security/v1"
	ContextOCI             = "https://opencontainers.org/ns/distribution"
)

type OCIManifest struct {
	Context       []string `json:"@context"`
	Type          string   `json:"type"`
	ID            string   `json:"id"`
	AttributedTo  string   `json:"attributedTo"`
	Published     string   `json:"published"`
	Repository    string   `json:"ociRepository"`
	Digest        string   `json:"ociDigest"`
	MediaType     string   `json:"ociMediaType"`
	Size          int64    `json:"ociSize"`
	Content       string   `json:"ociContent,omitempty"`
	SubjectDigest string   `json:"ociSubjectDigest,omitempty"`
	Tag           string   `json:"ociTag,omitempty"`
}

// EncodeContent base64-encodes manifest bytes for AP transport.
func EncodeContent(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

// DecodeContent decodes base64 manifest content from AP transport.
func DecodeContent(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(encoded)
}

type OCITag struct {
	Context      []string `json:"@context"`
	Type         string   `json:"type"`
	ID           string   `json:"id"`
	AttributedTo string   `json:"attributedTo"`
	Published    string   `json:"published"`
	Repository   string   `json:"ociRepository"`
	Tag          string   `json:"ociTag"`
	Digest       string   `json:"ociDigest"`
}

type OCIBlob struct {
	Context      []string `json:"@context"`
	Type         string   `json:"type"`
	ID           string   `json:"id"`
	AttributedTo string   `json:"attributedTo"`
	Published    string   `json:"published"`
	Digest       string   `json:"ociDigest"`
	Size         int64    `json:"ociSize"`
	Endpoint     string   `json:"ociEndpoint"`
}

func ociContext() []string {
	return []string{ContextActivityStreams, ContextSecurity, ContextOCI}
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
