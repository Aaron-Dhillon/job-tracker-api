package httpx_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Aaron-Dhillon/job-tracker-api/internal/httpx"
)

// decodeInto runs Decode against a request carrying body and reports what came
// back, so each test below is one line of setup and one of assertion.
func decodeInto(t *testing.T, body string, dst any) (*httptest.ResponseRecorder, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return rec, httpx.Decode(rec, req, dst)
}

type payload struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestWriteError_ShapeIsExactlyErrorKey(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteError(rec, http.StatusConflict, "email already registered")

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	// JSONEq would pass on an envelope with extra keys; the exact string is
	// what pins the shape PRD section Endpoints fixes.
	assert.Equal(t, `{"error":"email already registered"}`, strings.TrimSpace(rec.Body.String()))
}

func TestWriteJSON_EncodesValueAndStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteJSON(rec, http.StatusCreated, payload{Name: "Visa", Count: 2})

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"name":"Visa","count":2}`, rec.Body.String())
}

// A 204 carries no body by definition; WriteJSON must not write "null".
func TestWriteJSON_NilWritesNoBody(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteJSON(rec, http.StatusNoContent, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
}

// The status is on the wire before encoding starts, so a value that cannot
// marshal cannot be turned into a 500. WriteJSON must log and survive rather
// than panic mid-response.
func TestWriteJSON_UnmarshalableValueDoesNotPanic(t *testing.T) {
	rec := httptest.NewRecorder()

	assert.NotPanics(t, func() {
		httpx.WriteJSON(rec, http.StatusOK, map[string]any{"ch": make(chan int)})
	})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestDecode_Valid(t *testing.T) {
	var got payload
	_, err := decodeInto(t, `{"name":"Visa","count":2}`, &got)

	require.NoError(t, err)
	assert.Equal(t, payload{Name: "Visa", Count: 2}, got)
}

// The reason DisallowUnknownFields is on: a typo'd field silently becomes a
// zero value otherwise, and the caller gets a 201 for a row they did not mean
// to create.
func TestDecode_RejectsUnknownField(t *testing.T) {
	var got payload
	_, err := decodeInto(t, `{"name":"Visa","nmae":"typo"}`, &got)

	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, statusOf(t, err))
	assert.Contains(t, err.Error(), "unknown field")
	assert.Contains(t, err.Error(), "nmae")
}

func TestDecode_RejectsOversizedBody(t *testing.T) {
	var got payload
	huge := fmt.Sprintf(`{"name":%q}`, strings.Repeat("a", int(httpx.MaxBodyBytes)+1))
	require.Greater(t, int64(len(huge)), httpx.MaxBodyBytes)

	_, err := decodeInto(t, huge, &got)

	require.Error(t, err)
	assert.Equal(t, http.StatusRequestEntityTooLarge, statusOf(t, err),
		"an oversized body is 413, not 400 -- the request is well-formed, just too big")
	assert.Contains(t, err.Error(), "must not exceed")
}

// A body one byte under the cap must still decode, so the limit is proven to
// be a boundary rather than a blanket rejection.
func TestDecode_AcceptsBodyAtTheLimit(t *testing.T) {
	var got payload
	filler := strings.Repeat("a", int(httpx.MaxBodyBytes)-len(`{"name":""}`))
	body := fmt.Sprintf(`{"name":%q}`, filler)
	require.Equal(t, httpx.MaxBodyBytes, int64(len(body)))

	_, err := decodeInto(t, body, &got)

	require.NoError(t, err)
	assert.Len(t, got.Name, len(filler))
}

func TestDecode_ClassifiesMalformedBodies(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		status   int
		contains string
	}{
		{"empty body", ``, http.StatusBadRequest, "empty"},
		{"syntax error", `{"name":}`, http.StatusBadRequest, "malformed JSON"},
		{"truncated", `{"name":"Vis`, http.StatusBadRequest, "malformed JSON"},
		{"wrong field type", `{"count":"two"}`, http.StatusBadRequest, `field "count"`},
		{"array not object", `[1,2,3]`, http.StatusBadRequest, "must be of type"},
		{"trailing object", `{"name":"a"}{"name":"b"}`, http.StatusBadRequest, "single JSON object"},
		{"trailing garbage", `{"name":"a"} oops`, http.StatusBadRequest, "single JSON object"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got payload
			_, err := decodeInto(t, tt.body, &got)

			require.Error(t, err)
			assert.Equal(t, tt.status, statusOf(t, err))
			assert.Contains(t, err.Error(), tt.contains)
		})
	}
}

// Whatever the failure, the message reaching the client stays inside the
// documented envelope and carries the status Decode chose.
func TestWriteDecodeError_UsesCarriedStatus(t *testing.T) {
	var got payload
	huge := fmt.Sprintf(`{"name":%q}`, strings.Repeat("a", int(httpx.MaxBodyBytes)+1))
	rec, err := decodeInto(t, huge, &got)
	require.Error(t, err)

	httpx.WriteDecodeError(rec, err)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.JSONEq(t, fmt.Sprintf(`{"error":%q}`, err.Error()), rec.Body.String())
}

// An error from somewhere other than Decode must not reach the client verbatim
// -- that is how internal detail leaks into a response body.
func TestWriteDecodeError_GenericForForeignError(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteDecodeError(rec, fmt.Errorf("dial tcp 10.0.0.1:5432: connection refused"))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid request body"}`, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "10.0.0.1")
}

// statusOf pulls the status Decode attached to err, failing the test if the
// error is not one of ours.
func statusOf(t *testing.T, err error) int {
	t.Helper()
	var de *httpx.Error
	require.ErrorAs(t, err, &de, "Decode must return *httpx.Error so handlers get a status")
	return de.Status
}
