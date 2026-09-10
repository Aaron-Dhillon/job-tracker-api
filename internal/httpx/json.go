// Package httpx holds the HTTP plumbing every handler shares: JSON encoding,
// the {"error": "..."} response envelope, and request body decoding.
//
// It deliberately knows nothing about the domain. Handlers import it; it
// imports nothing of ours.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// MaxBodyBytes caps a request body at 1 MB. Every payload this API accepts is
// a handful of short fields, so anything larger is a mistake or an attempt to
// exhaust memory; the limit is enforced before decoding, not after.
const MaxBodyBytes int64 = 1 << 20

// ErrorBody is the error envelope, fixed by PRD §Endpoints. Handlers that need
// the documented 409 extension (from/allowed) embed this struct rather than
// inventing a second shape.
type ErrorBody struct {
	Error string `json:"error"`
}

// Error is a decode failure carrying the status the handler should return.
// Its message is safe to send to the client: it names the offending field but
// never echoes the field's value back.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// WriteJSON encodes v as the response body.
//
// The status is written before encoding, so an encoding failure part-way
// through cannot be turned into a 500 -- the header is already on the wire.
// All it can do is log; that is why v should always be a type that is known to
// marshal cleanly.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("httpx: encode response: %v", err)
	}
}

// WriteError writes the {"error": "..."} envelope with the given status.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorBody{Error: msg})
}

// WriteDecodeError writes the failure Decode returned, using the status it
// carries. Anything else becomes a 400 with a generic message, so an
// unexpected error can never leak internals into the response body.
func WriteDecodeError(w http.ResponseWriter, err error) {
	var de *Error
	if errors.As(err, &de) {
		WriteError(w, de.Status, de.Message)
		return
	}
	log.Printf("httpx: unclassified decode error: %v", err)
	WriteError(w, http.StatusBadRequest, "invalid request body")
}

// Decode reads a single JSON object from the request body into dst.
//
// Three things it does that a bare json.Decoder does not:
//
//   - caps the body at MaxBodyBytes via http.MaxBytesReader, which also closes
//     the connection rather than reading an unbounded stream into memory;
//   - rejects unknown fields, so a client typing "role_titel" gets a 400
//     instead of silently creating a row with an empty title;
//   - rejects trailing content, so {"a":1}{"b":2} is an error rather than a
//     silently ignored second object.
//
// Every error it returns is a *Error with a client-safe message; pass it to
// WriteDecodeError.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}

	// A second value means the body was not a single JSON object.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return &Error{Status: http.StatusBadRequest, Message: "body must contain a single JSON object"}
	}
	return nil
}

// decodeError translates encoding/json's errors into client-safe messages.
// Several of them are only distinguishable by their text, which is why the
// string prefix match below exists -- encoding/json exports no error type for
// an unknown field.
func decodeError(err error) *Error {
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		maxErr    *http.MaxBytesError
	)

	switch {
	case errors.As(err, &maxErr):
		return &Error{
			Status:  http.StatusRequestEntityTooLarge,
			Message: fmt.Sprintf("request body must not exceed %d bytes", maxErr.Limit),
		}

	case errors.As(err, &syntaxErr):
		return &Error{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("malformed JSON at byte %d", syntaxErr.Offset),
		}

	case errors.Is(err, io.ErrUnexpectedEOF):
		return &Error{Status: http.StatusBadRequest, Message: "malformed JSON: unexpected end of body"}

	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return &Error{
				Status:  http.StatusBadRequest,
				Message: fmt.Sprintf("field %q must be of type %s", typeErr.Field, typeErr.Type),
			}
		}
		return &Error{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("body must be of type %s", typeErr.Type),
		}

	case errors.Is(err, io.EOF):
		return &Error{Status: http.StatusBadRequest, Message: "request body is empty"}

	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.TrimPrefix(err.Error(), "json: unknown field ")
		return &Error{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("unknown field %s", field),
		}
	}

	return &Error{Status: http.StatusBadRequest, Message: "invalid request body"}
}
