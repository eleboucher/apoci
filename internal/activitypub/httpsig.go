package activitypub

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"time"

	"code.superseriousbusiness.org/httpsig"
	"github.com/jellydator/ttlcache/v3"
)

const signatureMaxAge = 5 * time.Minute

type SignatureCache struct {
	cache *ttlcache.Cache[string, struct{}]
}

func NewSignatureCache() *SignatureCache {
	cache := ttlcache.New[string, struct{}](
		ttlcache.WithTTL[string, struct{}](signatureMaxAge + 1*time.Minute),
	)
	go cache.Start()
	return &SignatureCache{cache: cache}
}

func (sc *SignatureCache) Stop() {
	sc.cache.Stop()
}

func (sc *SignatureCache) seen(keyID, signature string) bool {
	key := keyID + "\x00" + signature
	_, loaded := sc.cache.GetOrSet(key, struct{}{})
	return loaded
}

func SignRequest(req *http.Request, keyID string, privKey crypto.PrivateKey, body []byte) error {
	if req.Header.Get("Host") == "" && req.Host != "" {
		req.Header.Set("Host", req.Host)
	}
	if req.Header.Get("Date") == "" {
		req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}

	headers := []string{httpsig.RequestTarget, "host", "date"}
	if len(body) > 0 {
		headers = append(headers, "digest")
	}

	algo := algorithmForKey(privKey)

	signer, _, err := httpsig.NewSigner(
		[]httpsig.Algorithm{algo},
		httpsig.DigestSha256,
		headers,
		httpsig.Signature,
		int64(signatureMaxAge.Seconds()),
	)
	if err != nil {
		return fmt.Errorf("creating signer: %w", err)
	}
	return signer.SignRequest(privKey, keyID, req, body)
}

func algorithmForKey(key crypto.PrivateKey) httpsig.Algorithm {
	switch key.(type) {
	case *ecdsa.PrivateKey:
		return httpsig.ECDSA_SHA256
	case *rsa.PrivateKey:
		return httpsig.RSA_SHA256
	default:
		panic(fmt.Sprintf("unsupported private key type: %T", key))
	}
}

var requiredSignedHeaders = []string{"(request-target)", "host", "date"}

func VerifyRequest(req *http.Request, pubKeyPEM string, body []byte, sigCache *SignatureCache) error {
	verifier, err := httpsig.NewVerifier(req)
	if err != nil {
		return fmt.Errorf("verifying signature: %w", err)
	}

	signedHeaders, err := extractSignedHeaders(req)
	if err != nil {
		return err
	}

	hasHeader := func(name string) bool {
		for _, h := range signedHeaders {
			if strings.EqualFold(h, name) {
				return true
			}
		}
		return false
	}

	for _, required := range requiredSignedHeaders {
		if !hasHeader(required) {
			return fmt.Errorf("%s must be included in signed headers", required)
		}
	}

	if len(body) > 0 && !hasHeader("digest") {
		return fmt.Errorf("digest must be included in signed headers when body is present")
	}

	dateStr := req.Header.Get("Date")
	if dateStr == "" {
		return fmt.Errorf("missing Date header")
	}
	date, err := time.Parse(http.TimeFormat, dateStr)
	if err != nil {
		return fmt.Errorf("invalid Date header: %w", err)
	}
	age := time.Since(date)
	if age < 0 {
		age = -age
	}
	if age > signatureMaxAge {
		return fmt.Errorf("signature expired: Date header is %s old", age.Round(time.Second))
	}

	if len(body) > 0 {
		if err := verifyBodyDigest(req, body); err != nil {
			return err
		}
	}

	pubKey, err := parsePublicKeyPEM(pubKeyPEM)
	if err != nil {
		return fmt.Errorf("parsing public key: %w", err)
	}

	algo := algorithmForPublicKey(pubKey)
	if err := verifier.Verify(pubKey, algo); err != nil {
		return err
	}

	if sigCache != nil {
		keyID := verifier.KeyId()
		sig := extractRawSignature(req)
		if sig == "" {
			return fmt.Errorf("missing signature= field in Signature header")
		}
		if sigCache.seen(keyID, sig) {
			return fmt.Errorf("replayed signature detected")
		}
	}

	return nil
}

func extractRawSignature(req *http.Request) string {
	sig := req.Header.Get("Signature")
	for part := range strings.SplitSeq(sig, ",") {
		part = strings.TrimSpace(part)
		if val, ok := strings.CutPrefix(part, "signature="); ok {
			return strings.Trim(val, `"`)
		}
	}
	// No signature= field found — return empty so the cache check is skipped.
	return ""
}

func ExtractKeyID(req *http.Request) (string, error) {
	verifier, err := httpsig.NewVerifier(req)
	if err != nil {
		return "", fmt.Errorf("extracting key ID: %w", err)
	}
	return verifier.KeyId(), nil
}

// extractSignedHeaders parses the headers= field from the Signature header,
// rejecting duplicate headers= fields.
func extractSignedHeaders(req *http.Request) ([]string, error) {
	sig := req.Header.Get("Signature")
	if sig == "" {
		return nil, fmt.Errorf("missing Signature header")
	}

	var headersVal string
	found := false
	for part := range strings.SplitSeq(sig, ",") {
		part = strings.TrimSpace(part)
		if val, ok := strings.CutPrefix(part, "headers="); ok {
			if found {
				return nil, fmt.Errorf("duplicate headers= field in Signature header")
			}
			headersVal = strings.Trim(val, `"`)
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("headers= field missing from Signature header")
	}
	return strings.Fields(headersVal), nil
}

func verifyBodyDigest(req *http.Request, body []byte) error {
	d := req.Header.Get("Digest")
	if d == "" {
		return fmt.Errorf("digest header required when body is present")
	}
	if !strings.HasPrefix(d, "SHA-256=") {
		return fmt.Errorf("unsupported digest algorithm: %s", d)
	}
	h := sha256.Sum256(body)
	expected := "SHA-256=" + base64.StdEncoding.EncodeToString(h[:])
	if d != expected {
		return fmt.Errorf("body digest mismatch")
	}
	return nil
}

func parsePublicKeyPEM(pemStr string) (crypto.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	switch pub.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey:
		return pub, nil
	default:
		return nil, fmt.Errorf("unsupported key type: %T", pub)
	}
}

func algorithmForPublicKey(key crypto.PublicKey) httpsig.Algorithm {
	switch key.(type) {
	case *ecdsa.PublicKey:
		return httpsig.ECDSA_SHA256
	case *rsa.PublicKey:
		return httpsig.RSA_SHA256
	default:
		panic(fmt.Sprintf("unsupported public key type: %T", key))
	}
}
