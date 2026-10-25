package whatsapp

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	waStore "go.mau.fi/whatsmeow/store"
)

func TestVersionRefreshUpdatesHandshakeAndBoundsRequest(t *testing.T) {
	original := waStore.GetWAVersion()
	defer waStore.SetWAVersion(original)
	expected := waStore.WAVersionContainer{2, 3000, 1234567890}
	err := refreshWebVersion(context.Background(), func(ctx context.Context, client *http.Client) (*waStore.WAVersionContainer, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 20*time.Second {
			t.Fatal("version discovery must have a deadline")
		}
		return &expected, nil
	})
	if err != nil || waStore.GetWAVersion() != expected {
		t.Fatal("successful discovery must update the handshake version")
	}
}

func TestFailedVersionRefreshPreservesPreviousVersion(t *testing.T) {
	original := waStore.GetWAVersion()
	for _, fetch := range []func(context.Context, *http.Client) (*waStore.WAVersionContainer, error){
		func(context.Context, *http.Client) (*waStore.WAVersionContainer, error) {
			return nil, errors.New("synthetic unavailable")
		},
		func(context.Context, *http.Client) (*waStore.WAVersionContainer, error) { return nil, nil },
	} {
		if refreshWebVersion(context.Background(), fetch) == nil || waStore.GetWAVersion() != original {
			t.Fatal("failed discovery must leave the bundled version unchanged")
		}
	}
}
