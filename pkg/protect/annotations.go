package protect

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
)

const (
	// AnnotationSignature holds the base64 signature (or MAC) of the YAML layer.
	AnnotationSignature = "io.docker.agent.signature"
	// AnnotationSignatureAlgorithm names the algorithm behind AnnotationSignature.
	AnnotationSignatureAlgorithm = "io.docker.agent.signature.algorithm"
	// AnnotationEncrypted holds a base64 authenticated-encrypted copy of the
	// whole YAML layer.
	AnnotationEncrypted = "io.docker.agent.encrypted"
	// AnnotationEncryptedAlgorithm names the algorithm behind AnnotationEncrypted.
	AnnotationEncryptedAlgorithm = "io.docker.agent.encrypted.algorithm"
)

var (
	ErrNotProtected  = errors.New("artifact is neither signed nor encrypted")
	ErrNotEncrypted  = errors.New("artifact has no encrypted copy")
	ErrTampered      = errors.New("encrypted copy does not match the artifact content")
	ErrAlgorithmMism = errors.New("algorithm mismatch")
)

// Mode selects what the publisher records in the annotations.
type Mode string

const (
	// ModeSign records a signature (asymmetric key) or MAC (secret).
	// Holders of the matching public key or secret can verify integrity.
	ModeSign Mode = "sign"
	// ModeEncrypt records an encrypted copy of the whole YAML. Holders of the
	// secret or private key can both verify integrity and recover the YAML
	// from the annotation alone.
	ModeEncrypt Mode = "encrypt"
)

// Protect records the protection for data in annotations according to mode.
func (k *Key) Protect(annotations map[string]string, data []byte, mode Mode) error {
	switch mode {
	case ModeSign:
		if !k.CanSign() {
			return fmt.Errorf("%w (%s)", ErrCannotSign, k.Describe())
		}
		sig, err := k.Sign(data)
		if err != nil {
			return err
		}
		annotations[AnnotationSignature] = base64.StdEncoding.EncodeToString(sig)
		annotations[AnnotationSignatureAlgorithm] = k.SignAlgorithm()
		return nil
	case ModeEncrypt:
		if !k.CanEncrypt() {
			return fmt.Errorf("%w (%s)", ErrCannotEncrypt, k.Describe())
		}
		blob, err := k.Encrypt(data)
		if err != nil {
			return err
		}
		annotations[AnnotationEncrypted] = base64.StdEncoding.EncodeToString(blob)
		annotations[AnnotationEncryptedAlgorithm] = k.EncryptAlgorithm()
		return nil
	default:
		return fmt.Errorf("unknown protection mode %q", mode)
	}
}

// IsProtected reports whether annotations carry a signature or encrypted copy.
func IsProtected(annotations map[string]string) bool {
	return annotations[AnnotationSignature] != "" || annotations[AnnotationEncrypted] != ""
}

// VerifyAnnotations checks data against whatever protection annotations carry:
// the signature is verified, and the encrypted copy is decrypted and compared
// to data. ErrNotProtected is returned when neither is present.
func (k *Key) VerifyAnnotations(annotations map[string]string, data []byte) error {
	if !IsProtected(annotations) {
		return ErrNotProtected
	}
	if annotations[AnnotationSignature] != "" {
		if err := k.verifySignature(annotations, data); err != nil {
			return err
		}
	}
	if annotations[AnnotationEncrypted] != "" {
		plain, err := k.Recover(annotations)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(plain, data) != 1 {
			return ErrTampered
		}
	}
	return nil
}

func (k *Key) verifySignature(annotations map[string]string, data []byte) error {
	if !k.CanVerify() {
		return fmt.Errorf("%w: %s cannot verify signatures", ErrInvalidSignature, k.Describe())
	}
	if alg := annotations[AnnotationSignatureAlgorithm]; alg != k.SignAlgorithm() {
		return fmt.Errorf("%w: artifact signed with %q but key supports %q", ErrAlgorithmMism, alg, k.SignAlgorithm())
	}
	sig, err := base64.StdEncoding.DecodeString(annotations[AnnotationSignature])
	if err != nil {
		return fmt.Errorf("%w: malformed signature annotation: %w", ErrInvalidSignature, err)
	}
	return k.Verify(data, sig)
}

// Recover decrypts the encrypted copy carried in annotations, returning the
// clear YAML. It works from the annotations alone, without the layer.
func (k *Key) Recover(annotations map[string]string) ([]byte, error) {
	encoded := annotations[AnnotationEncrypted]
	if encoded == "" {
		return nil, ErrNotEncrypted
	}
	if !k.CanDecrypt() {
		return nil, fmt.Errorf("%w (%s)", ErrCannotDecrypt, k.Describe())
	}
	if alg := annotations[AnnotationEncryptedAlgorithm]; alg != k.EncryptAlgorithm() {
		return nil, fmt.Errorf("%w: artifact encrypted with %q but key supports %q", ErrAlgorithmMism, alg, k.EncryptAlgorithm())
	}
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed encrypted annotation: %w", ErrDecryption, err)
	}
	return k.Decrypt(blob)
}
