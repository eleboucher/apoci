package activitypub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
)

const keyBits = 2048

type Identity struct {
	ActorURL      string
	Domain        string
	AccountDomain string
	PrivateKey    *rsa.PrivateKey
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

func LoadOrCreateIdentity(domain, accountDomain, keyPath string, logger *slog.Logger) (*Identity, error) {
	if accountDomain == "" {
		accountDomain = domain
	}
	actorURL := "https://" + domain + "/ap/actor"

	var privKey *rsa.PrivateKey

	if keyPath != "" {
		data, err := os.ReadFile(keyPath) //nolint:gosec // keyPath is operator-configured
		if err == nil {
			privKey, err = parseRSAPrivateKey(data)
			if err != nil {
				return nil, fmt.Errorf("parsing private key from %s: %w", keyPath, err)
			}
			logger.Info("loaded existing RSA key", "path", keyPath)
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
		privKey, err = rsa.GenerateKey(rand.Reader, keyBits)
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

func parseRSAPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}

	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key is not RSA")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("unexpected PEM block type: %s", block.Type)
	}
}

func generateAndSaveKey(keyPath string, logger *slog.Logger) (*rsa.PrivateKey, error) {
	privKey, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, fmt.Errorf("generating RSA key: %w", err)
	}

	keyBytes := x509.MarshalPKCS1PrivateKey(privKey)
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: keyBytes,
	})

	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("saving key to %s: %w", keyPath, err)
	}

	logger.Info("generated new RSA key", "path", keyPath)
	return privKey, nil
}
