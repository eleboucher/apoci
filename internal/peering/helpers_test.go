package peering

import (
	"os"
	"testing"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

func TestMain(m *testing.M) {
	validate.AllowPrivateIPs = true
	os.Exit(m.Run())
}
