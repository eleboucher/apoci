package activitypub

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// FedErrorKind classifies federation errors so callers can choose the right
// HTTP response code and log level without string-matching error messages.
type FedErrorKind int

const (
	ErrMalformed       FedErrorKind = iota // 400 — bad ActivityPub payload
	ErrUnauthorized                        // 401 — signature verification failed
	ErrForbidden                           // 403 — blocked actor or domain
	ErrNotRelevant                         // 200 — activity doesn't apply (log at debug)
	ErrPeerUnreachable                     // 502 — remote resource fetch failed
)

// FedError is a typed federation error that carries an HTTP status code
// and an appropriate log level. Inspired by GoToSocial's WithCode error.
type FedError struct {
	Kind    FedErrorKind
	Message string
	Err     error
}

func (e *FedError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *FedError) Unwrap() error { return e.Err }

func (e *FedError) HTTPCode() int {
	switch e.Kind {
	case ErrMalformed:
		return http.StatusBadRequest
	case ErrUnauthorized:
		return http.StatusUnauthorized
	case ErrForbidden:
		return http.StatusForbidden
	case ErrNotRelevant:
		return http.StatusAccepted // silent accept — don't reveal irrelevance
	case ErrPeerUnreachable:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// ErrNotRelevant is debug-only; all other kinds log at warn.
func (e *FedError) LogLevel() slog.Level {
	if e.Kind == ErrNotRelevant {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}

// AsFedError extracts a *FedError from err if present.
func AsFedError(err error) (*FedError, bool) {
	var fe *FedError
	return fe, errors.As(err, &fe)
}
