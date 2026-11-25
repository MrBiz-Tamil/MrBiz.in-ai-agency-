package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestVerifyHMACSHA256Base64(t *testing.T) {
	secret := "test_secret_shopify_123"
	payload := []byte(`{"id": 9921, "email": "customer@example.com", "total_price": "420.00"}`)

	// Compute valid signature
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	validSignature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !VerifyHMACSHA256Base64(payload, secret, validSignature) {
		t.Errorf("expected signature to be valid")
	}

	// Tampered payload
	tamperedPayload := []byte(`{"id": 9921, "email": "hacker@example.com", "total_price": "0.00"}`)
	if VerifyHMACSHA256Base64(tamperedPayload, secret, validSignature) {
		t.Errorf("expected signature to fail on tampered payload")
	}

	// Wrong secret
	if VerifyHMACSHA256Base64(payload, "wrong_secret", validSignature) {
		t.Errorf("expected signature to fail on wrong secret")
	}
}

func TestVerifyHMACSHA256Hex(t *testing.T) {
	secret := "test_secret_hex_456"
	payload := []byte(`{"event": "charge.succeeded", "amount": 1200}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	validHex := hex.EncodeToString(mac.Sum(nil))

	if !VerifyHMACSHA256Hex(payload, secret, validHex) {
		t.Errorf("expected hex signature to be valid")
	}

	if VerifyHMACSHA256Hex(payload, secret, "invalid_hex_signature") {
		t.Errorf("expected invalid hex to fail")
	}
}

func TestComputePayloadDeduplicationKey(t *testing.T) {
	payload := []byte(`{"order_id": "ord_1023"}`)
	k1 := ComputePayloadDeduplicationKey("shopify", "ord_1023", payload)
	k2 := ComputePayloadDeduplicationKey("shopify", "ord_1023", payload)
	k3 := ComputePayloadDeduplicationKey("shopify", "ord_1024", payload)

	if k1 != k2 {
		t.Errorf("expected identical deduplication keys for same payload")
	}
	if k1 == k3 {
		t.Errorf("expected distinct deduplication keys for different external IDs")
	}
}
