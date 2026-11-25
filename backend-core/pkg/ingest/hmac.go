package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

var (
	ErrInvalidSignature = errors.New("invalid webhook signature")
	ErrEmptySecret      = errors.New("webhook secret cannot be empty")
)

// VerifyHMACSHA256Base64 checks standard base64-encoded HMAC-SHA256 signatures (e.g. Shopify).
func VerifyHMACSHA256Base64(payload []byte, secret string, signatureHeader string) bool {
	if secret == "" || signatureHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedMAC := mac.Sum(nil)

	actualMAC, err := base64.StdEncoding.DecodeString(signatureHeader)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(expectedMAC, actualMAC) == 1
}

// VerifyHMACSHA256Hex checks standard hex-encoded HMAC-SHA256 signatures (e.g. Stripe, BigCommerce).
func VerifyHMACSHA256Hex(payload []byte, secret string, signatureHeader string) bool {
	if secret == "" || signatureHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedMAC := mac.Sum(nil)

	actualMAC, err := hex.DecodeString(signatureHeader)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(expectedMAC, actualMAC) == 1
}

// ComputePayloadDeduplicationKey produces a deterministic SHA-256 fingerprint of the webhook content.
func ComputePayloadDeduplicationKey(source string, externalID string, payload []byte) string {
	h := sha256.New()
	h.Write([]byte(source))
	h.Write([]byte(":"))
	h.Write([]byte(externalID))
	h.Write([]byte(":"))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}
