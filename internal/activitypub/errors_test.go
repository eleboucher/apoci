package activitypub

import (
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFedErrorHTTPCode(t *testing.T) {
	tests := []struct {
		kind FedErrorKind
		want int
	}{
		{ErrMalformed, http.StatusBadRequest},
		{ErrUnauthorized, http.StatusUnauthorized},
		{ErrForbidden, http.StatusForbidden},
		{ErrNotRelevant, http.StatusAccepted},
		{ErrPeerUnreachable, http.StatusBadGateway},
	}
	for _, tt := range tests {
		fe := &FedError{Kind: tt.kind, Message: "test"}
		assert.Equal(t, tt.want, fe.HTTPCode(), "HTTPCode for kind %d", tt.kind)
	}
}

func TestFedErrorLogLevel(t *testing.T) {
	assert.Equal(t, slog.LevelDebug, (&FedError{Kind: ErrNotRelevant}).LogLevel())
	assert.Equal(t, slog.LevelWarn, (&FedError{Kind: ErrMalformed}).LogLevel())
	assert.Equal(t, slog.LevelWarn, (&FedError{Kind: ErrPeerUnreachable}).LogLevel())
}

func TestFedErrorUnwrap(t *testing.T) {
	inner := errors.New("connection refused")
	fe := &FedError{Kind: ErrPeerUnreachable, Message: "fetching actor", Err: inner}
	assert.ErrorIs(t, fe, inner)
}

func TestAsFedError(t *testing.T) {
	fe := &FedError{Kind: ErrMalformed, Message: "bad json"}
	wrapped := errors.New("wrapped: " + fe.Error())

	_, ok := AsFedError(wrapped)
	assert.False(t, ok, "plain error should not unwrap to FedError")

	got, ok := AsFedError(fe)
	assert.True(t, ok)
	assert.Equal(t, ErrMalformed, got.Kind)
}
