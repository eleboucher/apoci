package upstream

import (
	"testing"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

func TestMain(m *testing.M) {
	validate.AllowPrivateIPs.Store(true)
	m.Run()
}
