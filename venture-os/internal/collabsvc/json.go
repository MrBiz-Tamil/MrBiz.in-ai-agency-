package collabsvc

import (
	"encoding/json"
	"fmt"

	"github.com/mrbiz/venture-os/internal/collab"
)

func mustJSON(m map[string]string) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err) // a map[string]string always marshals
	}
	return b
}

func parseSecret(b []byte) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%w: stored credentials are unreadable", collab.ErrAuth)
	}
	return m, nil
}
