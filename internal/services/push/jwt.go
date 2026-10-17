package push

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// Both push providers authenticate with a short lived JWT rather than a static
// secret, so the signing helpers live here instead of pulling in a JOSE library.
// RS256 is used for Google's OAuth token endpoint and ES256 for APNs.

var (
	// ErrInvalidKey means a configured private key could not be parsed.
	ErrInvalidKey = errors.New("push: private key could not be parsed")
)

func b64url(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// signJWT builds a compact JWS from the given header and claims.
func signJWT(key crypto.PrivateKey, header, claims any) (string, error) {
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	signingInput := b64url(headerJSON) + "." + b64url(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))

	signature, err := sign(key, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(signature), nil
}

// sign produces a signature over digest in the format JWS expects. ECDSA
// signatures are DER encoded by crypto/ecdsa but JWS requires the fixed width
// r||s pair, so that case is converted here.
func sign(key crypto.PrivateKey, digest []byte) ([]byte, error) {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest)
	case *ecdsa.PrivateKey:
		der, err := ecdsa.SignASN1(rand.Reader, k, digest)
		if err != nil {
			return nil, err
		}
		return derToRaw(der)
	default:
		return nil, fmt.Errorf("push: unsupported key type %T", key)
	}
}

type ecdsaSignature struct {
	R, S *big.Int
}

// derToRaw converts a DER encoded ECDSA signature into the 64 byte r||s form.
func derToRaw(der []byte) ([]byte, error) {
	var parsed ecdsaSignature
	if _, err := asn1.Unmarshal(der, &parsed); err != nil {
		return nil, err
	}

	raw := make([]byte, 64)
	if parsed.R == nil || parsed.S == nil {
		return nil, ErrInvalidKey
	}
	// Fills the slice from the right, so keys shorter than 32 bytes still line up.
	parsed.R.FillBytes(raw[:32])
	parsed.S.FillBytes(raw[32:])
	return raw, nil
}

// parsePrivateKey decodes a PEM private key in PKCS#1, PKCS#8 or SEC1 form.
func parsePrivateKey(pemData string) (crypto.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, ErrInvalidKey
	}

	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, ErrInvalidKey
}

// tokenCache holds a minted access token until shortly before it expires, so a
// burst of notifications does not turn into a burst of auth requests.
type tokenCache struct {
	mu      sync.Mutex
	token   string
	expires time.Time
	now     func() time.Time
}

func newTokenCache() *tokenCache {
	return &tokenCache{now: time.Now}
}

// get returns the cached token, minting a new one via mint when it is missing or
// close to expiry.
func (c *tokenCache) get(mint func() (string, time.Time, error)) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Refresh a minute early so a token cannot expire mid flight.
	if c.token != "" && c.now().Add(time.Minute).Before(c.expires) {
		return c.token, nil
	}

	token, expires, err := mint()
	if err != nil {
		return "", err
	}
	c.token = token
	c.expires = expires
	return token, nil
}
