package httpserver

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
)

// withRecovery also covers applications that do not use the common Gin router.
// Panic values can contain credentials and must never be logged or returned.
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &recoveryResponseWriter{metricResponseWriter: &metricResponseWriter{ResponseWriter: w}}
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if writer.connection != nil {
				_ = writer.connection.Close()
			}
			if recovered == http.ErrAbortHandler {
				panic(http.ErrAbortHandler)
			}
			slog.ErrorContext(r.Context(), "http handler panic recovered", "error_code", "HTTP_HANDLER_PANIC")
			if writer.wroteHeader || writer.connection != nil {
				// A response that has started cannot safely be replaced by a 500.
				// Abort it without asking net/http to log the original panic value.
				panic(http.ErrAbortHandler)
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Cache-Control", "no-store")
			writer.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(writer).Encode(map[string]string{
				"code": "INTERNAL_ERROR", "message": "the request could not be completed", "request_id": RequestID(r.Context()),
			})
		}()
		next.ServeHTTP(writer, r)
	})
}

type recoveryResponseWriter struct {
	*metricResponseWriter
	connection net.Conn
}

func (writer *recoveryResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	connection, buffer, err := writer.metricResponseWriter.Hijack()
	if err == nil {
		writer.connection = connection
	}
	return connection, buffer, err
}

// Preserve ResponseController's access to the original streaming interfaces.
func (writer *recoveryResponseWriter) Unwrap() http.ResponseWriter {
	return writer.metricResponseWriter
}
