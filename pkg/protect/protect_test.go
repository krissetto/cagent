package protect

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

const payload = "version: \"2\"\nagents:\n  root:\n    model: auto\n"

func pemEncode(t *testing.T, typ string, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func pkcs8(t *testing.T, priv any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	return pemEncode(t, "PRIVATE KEY", der)
}

func pkix(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return pemEncode(t, "PUBLIC KEY", der)
}

type keyPair struct {
	priv, pub  []byte
	canSign    bool
	canEncrypt bool
	signAlg    string
	encAlg     string
}

// keyPairs returns every supported asymmetric key type and encoding.
func keyPairs(t *testing.T) map[string]keyPair {
	t.Helper()

	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	xPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)

	ecSEC1, err := x509.MarshalECPrivateKey(ecPriv)
	require.NoError(t, err)
	openssh, err := ssh.MarshalPrivateKey(edPriv, "")
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(edPub)
	require.NoError(t, err)

	ed := keyPair{canSign: true, signAlg: AlgEd25519}
	ec := keyPair{canSign: true, canEncrypt: true, signAlg: AlgECDSASHA256, encAlg: "ecies-p256-aes-256-gcm"}
	rs := keyPair{canSign: true, canEncrypt: true, signAlg: AlgRSAPSSSHA256, encAlg: AlgRSAOAEP}
	x := keyPair{canEncrypt: true, encAlg: "ecies-x25519-aes-256-gcm"}

	with := func(kp keyPair, priv, pub []byte) keyPair { kp.priv, kp.pub = priv, pub; return kp }

	return map[string]keyPair{
		"ed25519/pkcs8+pkix": with(ed, pkcs8(t, edPriv), pkix(t, edPub)),
		"ed25519/openssh":    with(ed, pem.EncodeToMemory(openssh), ssh.MarshalAuthorizedKey(sshPub)),
		"ecdsa/pkcs8+pkix":   with(ec, pkcs8(t, ecPriv), pkix(t, &ecPriv.PublicKey)),
		"ecdsa/sec1":         with(ec, pemEncode(t, "EC PRIVATE KEY", ecSEC1), pkix(t, &ecPriv.PublicKey)),
		"rsa/pkcs8+pkix":     with(rs, pkcs8(t, rsaPriv), pkix(t, &rsaPriv.PublicKey)),
		"rsa/pkcs1":          with(rs, pemEncode(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaPriv)), pemEncode(t, "RSA PUBLIC KEY", x509.MarshalPKCS1PublicKey(&rsaPriv.PublicKey))),
		"x25519/pkcs8+pkix":  with(x, pkcs8(t, xPriv), pkix(t, xPriv.PublicKey())),
	}
}

func TestParseKey_DetectsSymmetric(t *testing.T) {
	t.Parallel()

	key, err := ParseKey([]byte("s3cret\n"))
	require.NoError(t, err)
	assert.True(t, key.Symmetric())
	assert.True(t, key.CanSign())
	assert.True(t, key.CanEncrypt())
	assert.True(t, key.CanDecrypt())
	assert.Equal(t, AlgHMACSHA256, key.SignAlgorithm())
	assert.Equal(t, AlgAESGCM, key.EncryptAlgorithm())

	// Trailing newline must not change the key.
	same, err := ParseKey([]byte("s3cret"))
	require.NoError(t, err)
	assert.Equal(t, key.Fingerprint(), same.Fingerprint())
}

func TestParseKey_Errors(t *testing.T) {
	t.Parallel()

	_, err := ParseKey([]byte("  \n"))
	require.Error(t, err)

	_, err = ParseKey(pemEncode(t, "CERTIFICATE", []byte("junk")))
	require.ErrorContains(t, err, "unsupported PEM block type")

	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(edPriv, "", []byte("pw"))
	require.NoError(t, err)
	_, err = ParseKey(pem.EncodeToMemory(block))
	require.ErrorContains(t, err, "passphrase-protected")
}

func TestAsymmetric_Capabilities(t *testing.T) {
	t.Parallel()

	for name, kp := range keyPairs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			priv, err := ParseKey(kp.priv)
			require.NoError(t, err)
			pub, err := ParseKey(kp.pub)
			require.NoError(t, err)

			assert.False(t, priv.Symmetric())
			assert.True(t, priv.Private())
			assert.False(t, pub.Private())
			assert.Equal(t, priv.Fingerprint(), pub.Fingerprint())

			assert.Equal(t, kp.canSign, priv.CanSign())
			assert.False(t, pub.CanSign(), "public keys never sign")
			assert.Equal(t, kp.canSign, pub.CanVerify())
			assert.Equal(t, kp.signAlg, priv.SignAlgorithm())

			assert.Equal(t, kp.canEncrypt, priv.CanEncrypt())
			assert.Equal(t, kp.canEncrypt, pub.CanEncrypt(), "public keys can encrypt")
			assert.Equal(t, kp.canEncrypt, priv.CanDecrypt())
			assert.False(t, pub.CanDecrypt(), "public keys never decrypt")
			assert.Equal(t, kp.encAlg, priv.EncryptAlgorithm())
		})
	}
}

func TestAsymmetric_SignMode(t *testing.T) {
	t.Parallel()

	for name, kp := range keyPairs(t) {
		if !kp.canSign {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			priv, err := ParseKey(kp.priv)
			require.NoError(t, err)
			pub, err := ParseKey(kp.pub)
			require.NoError(t, err)

			annotations := map[string]string{}
			require.ErrorIs(t, pub.Protect(annotations, []byte(payload), ModeSign), ErrCannotSign)
			require.NoError(t, priv.Protect(annotations, []byte(payload), ModeSign))
			assert.Equal(t, kp.signAlg, annotations[AnnotationSignatureAlgorithm])
			assert.NotContains(t, annotations, AnnotationEncrypted)

			require.NoError(t, pub.VerifyAnnotations(annotations, []byte(payload)))
			require.NoError(t, priv.VerifyAnnotations(annotations, []byte(payload)))
			require.ErrorIs(t, pub.VerifyAnnotations(annotations, []byte(payload+"#\n")), ErrInvalidSignature)

			_, err = priv.Recover(annotations)
			require.ErrorIs(t, err, ErrNotEncrypted)
		})
	}
}

func TestAsymmetric_EncryptMode(t *testing.T) {
	t.Parallel()

	for name, kp := range keyPairs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			priv, err := ParseKey(kp.priv)
			require.NoError(t, err)
			pub, err := ParseKey(kp.pub)
			require.NoError(t, err)

			annotations := map[string]string{}
			if !kp.canEncrypt {
				require.ErrorIs(t, priv.Protect(annotations, []byte(payload), ModeEncrypt), ErrCannotEncrypt)
				return
			}

			// The publisher only needs the recipient's public key.
			require.NoError(t, pub.Protect(annotations, []byte(payload), ModeEncrypt))
			assert.Equal(t, kp.encAlg, annotations[AnnotationEncryptedAlgorithm])
			assert.NotContains(t, annotations, AnnotationSignature)

			// Only the private key can verify or recover.
			require.NoError(t, priv.VerifyAnnotations(annotations, []byte(payload)))
			require.ErrorIs(t, pub.VerifyAnnotations(annotations, []byte(payload)), ErrCannotDecrypt)
			require.ErrorIs(t, priv.VerifyAnnotations(annotations, []byte("tampered")), ErrTampered)

			plain, err := priv.Recover(annotations)
			require.NoError(t, err)
			assert.Equal(t, payload, string(plain))

			// A different key of the same type cannot decrypt.
			otherPriv, err := ParseKey(regenerate(t, kp))
			require.NoError(t, err)
			_, err = otherPriv.Recover(annotations)
			require.ErrorIs(t, err, ErrDecryption)
		})
	}
}

// regenerate returns a fresh private key of the same type as kp.
func regenerate(t *testing.T, kp keyPair) []byte {
	t.Helper()
	switch kp.encAlg {
	case AlgRSAOAEP:
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		return pkcs8(t, k)
	case "ecies-p256-aes-256-gcm":
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		return pkcs8(t, k)
	default:
		k, err := ecdh.X25519().GenerateKey(rand.Reader)
		require.NoError(t, err)
		return pkcs8(t, k)
	}
}

func TestSymmetric_BothModes(t *testing.T) {
	t.Parallel()

	key, err := ParseKey([]byte("correct horse battery staple"))
	require.NoError(t, err)
	other, err := ParseKey([]byte("wrong key"))
	require.NoError(t, err)

	signed := map[string]string{}
	require.NoError(t, key.Protect(signed, []byte(payload), ModeSign))
	require.NoError(t, key.VerifyAnnotations(signed, []byte(payload)))
	require.ErrorIs(t, key.VerifyAnnotations(signed, []byte("changed")), ErrInvalidSignature)
	require.ErrorIs(t, other.VerifyAnnotations(signed, []byte(payload)), ErrInvalidSignature)

	encrypted := map[string]string{}
	require.NoError(t, key.Protect(encrypted, []byte(payload), ModeEncrypt))
	require.NoError(t, key.VerifyAnnotations(encrypted, []byte(payload)))
	require.ErrorIs(t, key.VerifyAnnotations(encrypted, []byte("changed")), ErrTampered)
	_, err = other.Recover(encrypted)
	require.ErrorIs(t, err, ErrDecryption)

	plain, err := key.Recover(encrypted)
	require.NoError(t, err)
	assert.Equal(t, payload, string(plain))

	// Each encryption uses a fresh nonce.
	again := map[string]string{}
	require.NoError(t, key.Protect(again, []byte(payload), ModeEncrypt))
	assert.NotEqual(t, encrypted[AnnotationEncrypted], again[AnnotationEncrypted])
}

func TestVerifyAnnotations_Errors(t *testing.T) {
	t.Parallel()

	key, err := ParseKey([]byte("secret"))
	require.NoError(t, err)

	require.ErrorIs(t, key.VerifyAnnotations(map[string]string{}, []byte(payload)), ErrNotProtected)

	err = key.VerifyAnnotations(map[string]string{
		AnnotationSignature:          "AAAA",
		AnnotationSignatureAlgorithm: AlgEd25519,
	}, []byte(payload))
	require.ErrorIs(t, err, ErrAlgorithmMism)

	err = key.VerifyAnnotations(map[string]string{
		AnnotationSignature:          "not base64!",
		AnnotationSignatureAlgorithm: AlgHMACSHA256,
	}, []byte(payload))
	require.ErrorIs(t, err, ErrInvalidSignature)

	err = key.VerifyAnnotations(map[string]string{
		AnnotationEncrypted:          "AAAA",
		AnnotationEncryptedAlgorithm: AlgRSAOAEP,
	}, []byte(payload))
	require.ErrorIs(t, err, ErrAlgorithmMism)

	err = key.VerifyAnnotations(map[string]string{
		AnnotationEncrypted:          "AAAA",
		AnnotationEncryptedAlgorithm: AlgAESGCM,
	}, []byte(payload))
	require.ErrorIs(t, err, ErrDecryption)

	require.ErrorContains(t, key.Protect(map[string]string{}, nil, Mode("bogus")), "unknown protection mode")
}

func TestLoadKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0o600))

	key, err := LoadKey(path)
	require.NoError(t, err)
	assert.True(t, key.Symmetric())

	_, err = LoadKey(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}
