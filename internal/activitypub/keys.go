package activitypub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
)

type Identity struct {
	ActorURL      string
	Domain        string
	AccountDomain string
	PrivateKey    *ecdsa.PrivateKey
	Logger        *slog.Logger
}

func (id *Identity) PublicKeyPEM() (string, error) {
	pubASN1, err := x509.MarshalPKIXPublicKey(&id.PrivateKey.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshaling public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubASN1,
	})
	return string(pubPEM), nil
}

func (id *Identity) KeyID() string {
	return id.ActorURL + "#main-key"
}

func LoadOrCreateIdentity(endpoint, domain, accountDomain, keyPath string, logger *slog.Logger) (*Identity, error) {
	if accountDomain == "" {
		accountDomain = domain
	}
	actorURL := endpoint + "/ap/actor"

	var privKey *ecdsa.PrivateKey

	if keyPath != "" {
		data, err := os.ReadFile(keyPath) //nolint:gosec // keyPath is operator-configured
		if err == nil {
			privKey, err = parseECPrivateKey(data)
			if err != nil {
				return nil, fmt.Errorf("parsing private key from %s: %w", keyPath, err)
			}
			logger.Info("loaded existing ECDSA key", "path", keyPath)
		} else if os.IsNotExist(err) {
			privKey, err = generateAndSaveKey(keyPath, logger)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("reading key file %s: %w", keyPath, err)
		}
	} else {
		var err error
		privKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generating ephemeral key: %w", err)
		}
		logger.Warn("no key path configured, using ephemeral key (identity won't survive restart)")
	}

	logger.Info("activitypub identity ready", "actorURL", actorURL)

	return &Identity{
		ActorURL:      actorURL,
		Domain:        domain,
		AccountDomain: accountDomain,
		PrivateKey:    privKey,
		Logger:        logger,
	}, nil
}

func parseECPrivateKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}

	switch block.Type {
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		return nil, fmt.Errorf("key file contains an RSA key; delete it and restart to generate a new ECDSA key")
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		ecKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key is not ECDSA (got %T); delete the key file and restart to generate a new ECDSA key", key)
		}
		return ecKey, nil
	default:
		return nil, fmt.Errorf("unexpected PEM block type: %s", block.Type)
	}
}

func generateAndSaveKey(keyPath string, logger *slog.Logger) (*ecdsa.PrivateKey, error) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating ECDSA key: %w", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("marshaling ECDSA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyBytes,
	})

	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("saving key to %s: %w", keyPath, err)
	}

	logger.Info("generated new ECDSA P-256 key", "path", keyPath)
	return privKey, nil
}
