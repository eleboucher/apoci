package peering

import (
	"os"
	"testing"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

func TestMain(m *testing.M) {
	validate.AllowPrivateIPs.Store(true)
	os.Exit(m.Run())
}
