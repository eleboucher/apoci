package activitypub

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/apoci/apoci/internal/validate"
)

const testActorURL = "https://test.example.com/ap/actor"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMain(m *testing.M) {
	AllowInsecureHTTP = true
	validate.AllowPrivateIPs = true
	os.Exit(m.Run())
}
