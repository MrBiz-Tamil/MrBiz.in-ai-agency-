package mcpbus_test

import (
	"net/http"
	"testing"
)

// The authentication scheme is case-insensitive in HTTP: "bearer <token>" is a valid header, and the bus used to refuse it.
func TestBearerSchemeIsCaseInsensitiveOnTheBus(t *testing.T) {
	h := newHarness(t, nil)
	const ping = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		resp, _ := h.post(t, "", ping, map[string]string{"Authorization": scheme + " " + h.token})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("scheme %q: status %d", scheme, resp.StatusCode)
		}
	}
	for _, bad := range []string{"Basic " + h.token, "Bearer", "Bearer ", h.token} {
		resp, _ := h.post(t, "", ping, map[string]string{"Authorization": bad})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("header %q: status %d, want 401", bad, resp.StatusCode)
		}
	}
}
