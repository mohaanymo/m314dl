package httpx

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// BearerAuth wraps a handler with "Authorization: Bearer <secret>" checking.
// An empty secret disables auth (loopback-only binds).
func BearerAuth(secret string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h(w, r)
	}
}

// WriteJSON answers with v as JSON.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
