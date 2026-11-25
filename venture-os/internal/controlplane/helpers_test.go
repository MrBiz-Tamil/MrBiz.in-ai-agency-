package controlplane_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/ports"
)

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func transient() error { return fmt.Errorf("model overloaded: %w", ports.ErrTransient) }

func errPermanent() error { return errors.New("erpnext exploded: stack trace with secrets") }
