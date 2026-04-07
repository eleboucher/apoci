package activitypub

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

const testActorURL = "https://test.example.com/ap/actor"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMain(m *testing.M) {
	SetAllowInsecureHTTP(true)
	validate.AllowPrivateIPs = true
	os.Exit(m.Run())
}
