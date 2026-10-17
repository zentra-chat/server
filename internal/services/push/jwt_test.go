package push

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
)

// APNs rejects a token whose signature is not the fixed width r||s pair, so the
// DER to raw conversion is worth pinning down against a real signature.
func TestDerToRawProducesVerifiableSignature(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	digest := sha256.Sum256([]byte("payload"))
	der, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	raw, err := derToRaw(der)
	if err != nil {
		t.Fatalf("converting signature: %v", err)
	}
	if len(raw) != 64 {
		t.Fatalf("expected a 64 byte signature, got %d", len(raw))
	}

	r := new(big.Int).SetBytes(raw[:32])
	s := new(big.Int).SetBytes(raw[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Fatal("converted signature does not verify against the public key")
	}
}

func TestSignJWTIsVerifiable(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating rsa key: %v", err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating ec key: %v", err)
	}

	tests := []struct {
		name string
		key  any
	}{
		{name: "rsa", key: rsaKey},
		{name: "ecdsa", key: ecKey},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := signJWT(tt.key, map[string]string{"alg": "test"}, map[string]any{"iss": "zentra"})
			if err != nil {
				t.Fatalf("signing jwt: %v", err)
			}

			parts := strings.Split(token, ".")
			if len(parts) != 3 {
				t.Fatalf("expected a three part compact JWS, got %d parts", len(parts))
			}

			signature, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Fatalf("signature is not base64url: %v", err)
			}

			digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			switch key := tt.key.(type) {
			case *rsa.PrivateKey:
				if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
					t.Fatalf("rsa signature does not verify: %v", err)
				}
			case *ecdsa.PrivateKey:
				// ES256 always produces exactly 64 bytes.
				if len(signature) != 64 {
					t.Fatalf("expected a 64 byte ES256 signature, got %d", len(signature))
				}
			}
		})
	}
}

func TestParsePrivateKeyAcceptsPKCS8AndSEC1(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	sec1 := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})

	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatalf("marshalling pkcs8 key: %v", err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})

	for name, encoded := range map[string]string{"sec1": string(sec1), "pkcs8": string(pkcs8)} {
		t.Run(name, func(t *testing.T) {
			parsed, err := parsePrivateKey(encoded)
			if err != nil {
				t.Fatalf("parsing key: %v", err)
			}
			if _, ok := parsed.(*ecdsa.PrivateKey); !ok {
				t.Fatalf("expected an ECDSA key, got %T", parsed)
			}
		})
	}

	t.Run("garbage", func(t *testing.T) {
		if _, err := parsePrivateKey("not a pem block"); err == nil {
			t.Fatal("expected an error for an unparseable key")
		}
	})
}

func TestDataPayloadOmitsEmptyFields(t *testing.T) {
	msg := Message{
		Title:          "Someone replied to your message",
		Body:           "hello there",
		NotificationID: "abc",
		Type:           "reply",
		ChannelID:      "chan",
	}

	payload := msg.dataPayload()

	if payload["title"] != "Someone replied to your message" {
		t.Errorf("unexpected title: %q", payload["title"])
	}
	if payload["body"] != "hello there" {
		t.Errorf("unexpected body: %q", payload["body"])
	}
	// Absent fields are dropped, since FCM rejects empty data values in some
	// cases and the client treats "" as missing anyway.
	if _, present := payload["conversationId"]; present {
		t.Error("expected conversationId to be omitted when empty")
	}
	if _, present := payload["body"]; !present {
		t.Error("expected body to be present when set")
	}
}

func TestDataPayloadOmitsBodyWhenEmpty(t *testing.T) {
	payload := Message{Title: "You were mentioned"}.dataPayload()
	if _, present := payload["body"]; present {
		t.Error("expected body to be omitted when the notification has none")
	}
}
