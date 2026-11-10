package httpd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
)

const rcaRawPath = "/api/v1/rca/otlp/v1/logs"
const rcaRawBodyLimit = int64(1 << 20)

type rcaConnKey struct{}

func withRCARawConn(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, rcaConnKey{}, conn)
}
func isRCARawPath(path string) bool { return strings.TrimSuffix(path, "/") == rcaRawPath }
func isRCARawRequest(r *http.Request) bool {
	return isRCARawPath(r.URL.Path) || isRCARawPath(r.URL.EscapedPath())
}

// This is a status-only response boundary: no inner body or arbitrary headers
// are retained or sent. In particular inner WriteHeader cannot trigger the
// standard HTTP/1 unread-body drain before the transport decision.
type rcaRawResponse struct {
	header http.Header
	status int
}

func (w *rcaRawResponse) Header() http.Header { return w.header }
func (w *rcaRawResponse) WriteHeader(status int) {
	if w.status == 0 && status >= 200 && status <= 599 {
		w.status = status
	}
}
func (w *rcaRawResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return len(b), nil
}

type rcaRawBody struct {
	io.Reader
	remaining int64
}

func (b *rcaRawBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// Only the owning transport may close raw HTTP/1. Calling the underlying Close
// before hijack could cause a standard-library cleanup drain.
func (b *rcaRawBody) Close() error { return nil }

func rcaRawNative(r *http.Request) bool {
	if requestscope.IsLAN(r.Context()) || len(r.Header.Values("Origin")) != 0 {
		return false
	}
	for _, key := range []string{"Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-User", "Referer"} {
		if len(r.Header.Values(key)) != 0 {
			return false
		}
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	remote := net.ParseIP(peer)
	return (host == "localhost" || ip != nil && ip.IsLoopback()) && remote != nil && remote.IsLoopback()
}

type rcaRawLifetime struct {
	release func()
	once    sync.Once
}

func (l *rcaRawLifetime) register(release func()) bool {
	if l.release != nil || release == nil {
		return false
	}
	l.release = release
	return true
}
func (l *rcaRawLifetime) finish() {
	l.once.Do(func() {
		if l.release != nil {
			l.release()
		}
	})
}

func rcaRawTransportGuardWithReject(reject func(string, string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isRCARawRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			// Native raw diagnostics never echo an incoming request-id/header value.
			requestID := uuid.NewString()
			r = r.WithContext(context.WithValue(r.Context(), middleware.RequestIDKey, requestID))
			w.Header().Set(middleware.RequestIDHeader, requestID)
			if r.ProtoMajor >= 2 {
				rejectRCARawStream(w, r)
				return
			}
			if !rcaRawNative(r) {
				rejectRCARawHTTP1(w, r, http.StatusForbidden)
				return
			}
			rejectCapture := func(reason string) {
				markers := r.Header.Values("X-AO-RCA-Capture")
				if reject != nil && len(markers) == 1 {
					reject(markers[0], reason)
				}
			}
			if r.Method != http.MethodPost {
				rejectCapture("unknown_schema")
				rejectRCARawHTTP1(w, r, http.StatusMethodNotAllowed)
				return
			}
			if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
				rejectCapture("invalid_otlp")
				rejectRCARawHTTP1(w, r, http.StatusLengthRequired)
				return
			}
			if r.ContentLength > rcaRawBodyLimit {
				rejectCapture("batch_limit")
				rejectRCARawHTTP1(w, r, http.StatusRequestEntityTooLarge)
				return
			}
			encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
			if len(r.Header.Values("Content-Encoding")) > 1 || encoding != "" && encoding != "identity" || len(r.Header.Values("Content-Type")) != 1 || strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))) != "application/x-protobuf" {
				rejectCapture("unknown_schema")
				rejectRCARawHTTP1(w, r, http.StatusUnsupportedMediaType)
				return
			}
			controller := http.NewResponseController(w)
			if controller.SetReadDeadline(time.Now().Add(2*time.Second)) != nil {
				rejectCapture("unsupported_capability")
				rejectRCARawHTTP1(w, r, http.StatusServiceUnavailable)
				return
			}
			conn, _ := r.Context().Value(rcaConnKey{}).(net.Conn)
			var mu sync.Mutex
			active := true
			interrupt := func() {
				mu.Lock()
				defer mu.Unlock()
				if active && controller.SetReadDeadline(time.Now()) != nil {
					if conn != nil {
						_ = conn.Close()
					}
				}
			}
			stop := context.AfterFunc(r.Context(), interrupt)
			deactivate := func() { stop(); mu.Lock(); active = false; mu.Unlock() }
			defer deactivate()
			lifetime := &rcaRawLifetime{}
			defer func() {
				defer lifetime.finish()
				if value := recover(); value != nil {
					if panicErr, ok := value.(error); ok && errors.Is(panicErr, http.ErrAbortHandler) {
						panic(http.ErrAbortHandler)
					}
					rejectCapture("analysis_failed")
					rejectRCARawHTTP1(w, r, http.StatusInternalServerError)
				}
			}()
			r = r.WithContext(requestscope.WithRCARawFinished(r.Context(), lifetime.register))
			body := &rcaRawBody{Reader: r.Body, remaining: r.ContentLength}
			r.Body = body
			r = r.WithContext(requestscope.WithRCARawInterrupt(r.Context(), interrupt))
			response := &rcaRawResponse{header: make(http.Header)}
			next.ServeHTTP(response, r)
			deactivate()
			if response.status != http.StatusOK || body.remaining != 0 {
				status := response.status
				if status < 400 || status > 599 {
					status = http.StatusBadRequest
				}
				rejectRCARawHTTP1(w, r, status)
				return
			}
			if controller.SetReadDeadline(time.Time{}) != nil || controller.SetWriteDeadline(time.Now().Add(time.Second)) != nil {
				rejectRCARawHTTP1(w, r, http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.Header().Set("Content-Length", "0")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			if controller.Flush() != nil {
				panic(http.ErrAbortHandler)
			}
			_ = controller.SetWriteDeadline(time.Time{})
		})
	}
}
func rawRejectedError(r *http.Request, status int) envelope.APIError {
	value := envelope.APIError{Error: "bad_request", Code: "RCA_RAW_REJECTED", Message: "RCA raw batch rejected", RequestID: middleware.GetReqID(r.Context())}
	if status == http.StatusNotFound {
		value.Error, value.Code, value.Message = "not_found", "NOT_FOUND", "Route not found"
	}
	return value
}

// HTTP/1 raw rejection owns termination. Never read the hijacked buffer reader
// or close/discard r.Body. Missing terminal capabilities abort the request rather
// than returning through ordinary HTTP cleanup on a live connection.
func rejectRCARawHTTP1(w http.ResponseWriter, r *http.Request, status int) {
	if hijacker, ok := w.(http.Hijacker); ok {
		conn, buffer, err := hijacker.Hijack()
		if err == nil {
			defer func() { _ = conn.Close() }()
			if conn.SetReadDeadline(time.Now()) != nil || conn.SetWriteDeadline(time.Now().Add(time.Second)) != nil {
				return
			}
			body, _ := json.Marshal(rawRejectedError(r, status))
			_, _ = fmt.Fprintf(buffer, "HTTP/1.1 %d %s\r\nContent-Type: application/json; charset=utf-8\r\nContent-Length: %d\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n", status, http.StatusText(status), len(body))
			_, _ = buffer.Write(body)
			_ = buffer.Flush()
			return
		}
	}
	if conn, ok := r.Context().Value(rcaConnKey{}).(net.Conn); ok {
		_ = conn.Close()
	}
	panic(http.ErrAbortHandler)
}

// A LAN/tunnel h2 rejection must never touch the ConnContext close handle or
// connection deadlines: only this stream's pipe/writer belongs to the request.
func rejectRCARawStream(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	if http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second)) != nil {
		panic(http.ErrAbortHandler)
	}
	notFoundJSON(w, r)
}
