// Package protect signs or encrypts agent YAML shipped in OCI artifacts and
// verifies it on the way back.
//
// A key is either a symmetric secret or an asymmetric key (private, or
// public-only). The kind is detected from the file contents: PEM-encoded
// (RFC 7468) or OpenSSH keys are asymmetric; anything else is a raw secret.
//
// The YAML always stays in clear in the artifact layer. Depending on the
// Mode chosen by the publisher, the manifest annotations carry either a
// signature (MAC or digital signature) or an authenticated encrypted copy of
// the whole YAML that key holders can decrypt.
package protect

import (
	"bytes"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Key is a symmetric secret or an asymmetric key (private, or public-only).
type Key struct {
	secret []byte
	// priv is nil for public-only keys. One of: *rsa.PrivateKey,
	// *ecdsa.PrivateKey, ed25519.PrivateKey, *ecdh.PrivateKey.
	priv crypto.PrivateKey
	// pub is always set for asymmetric keys.
	pub crypto.PublicKey
}

// LoadKey reads and parses a key file. See ParseKey.
func LoadKey(path string) (*Key, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading key file: %w", err)
	}
	key, err := ParseKey(data)
	if err != nil {
		return nil, fmt.Errorf("parsing key file %s: %w", path, err)
	}
	return key, nil
}

// ParseKey detects the key kind from its encoding. PEM blocks and OpenSSH
// authorized_keys lines are parsed as asymmetric keys; any other content is a
// raw symmetric secret (surrounding whitespace is trimmed so a trailing
// newline does not change the key).
func ParseKey(data []byte) (*Key, error) {
	if block, _ := pem.Decode(data); block != nil {
		return parsePEM(block, data)
	}
	if pub, _, _, _, err := ssh.ParseAuthorizedKey(data); err == nil {
		cpk, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return nil, fmt.Errorf("unsupported ssh public key type %s", pub.Type())
		}
		return fromPublic(cpk.CryptoPublicKey())
	}

	secret := bytes.TrimSpace(data)
	if len(secret) == 0 {
		return nil, errors.New("key is empty")
	}
	return &Key{secret: secret}, nil
}

func parsePEM(block *pem.Block, data []byte) (*Key, error) {
	switch {
	case strings.Contains(block.Type, "ENCRYPTED"):
		return nil, errors.New("passphrase-protected keys are not supported")
	case strings.HasSuffix(block.Type, "PRIVATE KEY"):
		// Handles PKCS#1, SEC1, PKCS#8 (incl. X25519) and OpenSSH private keys.
		priv, err := ssh.ParseRawPrivateKey(data)
		if err != nil {
			if _, missing := errors.AsType[*ssh.PassphraseMissingError](err); missing {
				return nil, errors.New("passphrase-protected keys are not supported")
			}
			return nil, fmt.Errorf("parsing private key: %w", err)
		}
		return fromPrivate(priv)
	case block.Type == "PUBLIC KEY":
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing public key: %w", err)
		}
		return fromPublic(pub)
	case block.Type == "RSA PUBLIC KEY":
		pub, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing RSA public key: %w", err)
		}
		return fromPublic(pub)
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q", block.Type)
	}
}

func fromPrivate(priv any) (*Key, error) {
	switch p := priv.(type) {
	case *rsa.PrivateKey:
		return &Key{priv: p, pub: &p.PublicKey}, nil
	case *ecdsa.PrivateKey:
		return &Key{priv: p, pub: &p.PublicKey}, nil
	case ed25519.PrivateKey:
		return &Key{priv: p, pub: p.Public()}, nil
	case *ed25519.PrivateKey:
		return fromPrivate(*p)
	case *ecdh.PrivateKey:
		return &Key{priv: p, pub: p.PublicKey()}, nil
	default:
		return nil, fmt.Errorf("unsupported private key type %T", priv)
	}
}

func fromPublic(pub crypto.PublicKey) (*Key, error) {
	switch pub.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey, *ecdh.PublicKey:
		return &Key{pub: pub}, nil
	default:
		return nil, fmt.Errorf("unsupported public key type %T", pub)
	}
}

// Symmetric reports whether the key is a raw secret.
func (k *Key) Symmetric() bool { return k.secret != nil }

// Private reports whether the key holds private material (a secret or a
// private key), as opposed to a public-only key.
func (k *Key) Private() bool { return k.Symmetric() || k.priv != nil }

// Fingerprint returns a stable hex identifier for the key material: SHA-256 of
// the secret, or of the PKIX-encoded public key. Private and public halves of
// the same pair share a fingerprint.
func (k *Key) Fingerprint() string {
	material := k.secret
	if !k.Symmetric() {
		// Only fails for unsupported types, which fromPublic rejects.
		material, _ = x509.MarshalPKIXPublicKey(k.pub)
	}
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:])
}

// Describe returns a short human-readable description of the key.
func (k *Key) Describe() string {
	if k.Symmetric() {
		return "symmetric secret"
	}
	half := "public"
	if k.priv != nil {
		half = "private"
	}
	switch p := k.pub.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA-%d %s key", p.N.BitLen(), half)
	case *ecdsa.PublicKey:
		return fmt.Sprintf("ECDSA %s %s key", p.Curve.Params().Name, half)
	case ed25519.PublicKey:
		return "Ed25519 " + half + " key"
	case *ecdh.PublicKey:
		return fmt.Sprintf("%s %s key", p.Curve(), half)
	default:
		return "unknown key"
	}
}
