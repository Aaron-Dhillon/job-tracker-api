package httpx

import (
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// RequestIDHeader is the response header carrying chi's request id.
const RequestIDHeader = "X-Request-Id"

// EchoRequestID copies the request id chi's middleware.RequestID put on the
// context into the response header.
//
// The id is in every log line the server writes, so returning it is what turns
// "the API gave me a 500" into a grep. It goes in a header rather than the
// body because PRD §Endpoints fixes the error shape at {"error": "..."} --
// adding a field would break the contract for a debugging aid.
//
// Mount it after middleware.RequestID; without that it is a no-op rather than
// an error, which is the right failure for a diagnostic.
func EchoRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := middleware.GetReqID(r.Context()); id != "" {
			w.Header().Set(RequestIDHeader, id)
		}
		next.ServeHTTP(w, r)
	})
}
