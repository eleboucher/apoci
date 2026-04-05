package peering

import (
	"os"
	"testing"

	"github.com/apoci/apoci/internal/validate"
)

func TestMain(m *testing.M) {
	validate.AllowPrivateIPs = true
	os.Exit(m.Run())
}
