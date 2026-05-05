# Understanding a Go HTTP Middleware Test with slog

A walkthrough of a Go test case that verifies a request-logging middleware, focused on the parts that aren't immediately obvious.

## The Code Under Discussion

```go
package main_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func loggerMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			logger.Info("Served request",
				"method", r.Method,
				"path", r.URL.Path,
				"client_ip", r.RemoteAddr,
			)
		})
	}
}

func Test_requestLogger(t *testing.T) {
	logBuffer := &bytes.Buffer{}

	logger := slog.New(slog.NewTextHandler(logBuffer, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Time(slog.TimeKey, time.Date(2023, 10, 1, 12, 34, 57, 0, time.UTC))
			}
			return a
		},
	}))

	requestLoggerMiddleware := loggerMiddleware(logger)
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	loggedHandler := requestLoggerMiddleware(dummyHandler)

	req := httptest.NewRequest("GET", "http://lin.ko/api/stats", nil)
	rr := httptest.NewRecorder()
	loggedHandler.ServeHTTP(rr, req)

	const expectedLogString = `time=2023-10-01T12:34:57.000Z level=INFO msg="Served request" method=GET path=/api/stats client_ip=192.0.2.1:1234` + "\n"
	const expectedStatusCode = http.StatusOK

	if logBuffer.String() != expectedLogString {
		t.Errorf("FAIL")
	}

	if rr.Code != expectedStatusCode {
		t.Error("FAIL")
	}
}
```

## What the Test Actually Does (high level)

The middleware wraps any HTTP handler and, after that handler runs, logs the request method, path, and client IP. The test verifies that wrapping works correctly: it sends a fake GET request to `/api/stats` through the middleware, captures the log line, and asserts the line is exactly what's expected. It also checks the status code is 200.

To make the string comparison deterministic, the test pins the timestamp to a fixed value. Otherwise the log would contain a different `time=...` on every run and you'd never get a stable match.

---

## Question 1: Why is the log buffer a pointer (`&bytes.Buffer{}`)?

Two reasons, both important.

**First**, `bytes.Buffer`'s `Write` method has a pointer receiver. `slog.NewTextHandler` wants an `io.Writer`, and only `*bytes.Buffer` satisfies that interface, not the value type `bytes.Buffer`. Without the `&`, it wouldn't compile.

**Second**, even setting interfaces aside, if you passed the buffer by value the handler would write into a copy. Your later `logBuffer.String()` call would read from a different buffer that never received any writes. You need the handler (writing) and the test (reading) to share the same underlying memory.

So the pointer is both required by the interface and necessary for writes and reads to point at the same data.

---

## Question 2: What is `ReplaceAttr` doing? What are groups, attributes, TimeKey?

`slog` represents every log record as a series of key-value pairs called **attributes** (`slog.Attr`). When you write:

```go
logger.Info("Served request", "method", r.Method, ...)
```

slog turns those varargs into attributes like `{Key: "method", Value: "GET"}`. The handler also injects built-in attributes for time, level, and message.

**`ReplaceAttr`** is a hook the handler calls for every attribute before it gets formatted into output. From the hook you can return:

* the same attribute unchanged (pass through)
* a modified attribute (rewrite it)
* an empty `slog.Attr{}` (drop it entirely)

In this test, the hook checks whether the current attribute's key equals `slog.TimeKey`. `slog.TimeKey` is just the constant string `"time"`, the standard key slog uses for the timestamp attribute. When that attribute comes through, the hook replaces its value with a hard-coded `2023-10-01 12:34:57 UTC`. Every other attribute (`level`, `msg`, `method`, `path`, `client_ip`) is returned unchanged. This is the only trick that makes the test deterministic.

The other built-in keys, for reference, are `slog.LevelKey` (`"level"`), `slog.MessageKey` (`"msg"`), and `slog.SourceKey` (`"source"`).

### Groups, explained properly

Groups are basically namespaces for your log attributes. Think of them like folders.

By default, every attribute sits at the top level:

```go
logger.Info("hello", "user", "alice")
// output: time=... level=INFO msg=hello user=alice
```

But you can nest attributes inside a named group using `WithGroup`:

```go
logger.WithGroup("request").Info("hello", "user", "alice")
// output: time=... level=INFO msg=hello request.user=alice
```

Notice the `user` attribute is now `request.user`. It lives "inside" the request group. You can nest further:

```go
logger.WithGroup("request").WithGroup("headers").Info("hello", "auth", "Bearer xyz")
// output: time=... level=INFO msg=hello request.headers.auth="Bearer xyz"
```

When the handler calls your `ReplaceAttr` hook for the `auth` attribute above, it passes `groups = ["request", "headers"]` so your hook knows exactly where in the hierarchy that attribute sits. For the very first example with no groups at all, `groups` is just an empty slice `[]string{}`.

**Why would you care?** Because you might want to handle the same key differently depending on its location. For example, redact `auth` only when it appears inside the `headers` group, or strip out a `password` field only at the top level. The `groups` parameter is how you tell those cases apart inside `ReplaceAttr`.

In this test there are no `WithGroup` calls anywhere, so every attribute is top-level and `groups` is always empty. The hook ignores it entirely. It only exists in the function signature because slog requires that signature for any `ReplaceAttr` hook.

---

## Question 3: What is the recorder and why are we sending logs to an arbitrary link?

Nothing actually goes over the network. The whole test runs in memory.

**`httptest.NewRequest(...)`** fabricates an `*http.Request` object as if a client had sent it. The string `http://lin.ko/api/stats` is just parsed to populate `r.URL`. No DNS lookup, no TCP connection. The handler only reads `r.Method` and `r.URL.Path`, so the host could be literally anything. A real domain would behave identically here.

**`httptest.NewRecorder()`** is a fake `http.ResponseWriter`. A real response writer pushes bytes down a network connection. The recorder just buffers everything in memory so you can inspect it after the handler returns. It captures:

* the status code (`rr.Code`, which defaults to 200 if the handler never calls `WriteHeader`)
* the response headers (`rr.Header()`)
* the response body (`rr.Body`)

When `loggedHandler.ServeHTTP(rr, req)` runs, the middleware passes through to the dummy handler (which does nothing, so status stays at the default 200), then writes the log line into your captured buffer. After that single call returns, the test inspects both the recorded response and the captured log output.

One small detail worth knowing: `httptest.NewRequest` sets `RemoteAddr` to `192.0.2.1:1234` by default. That's the TEST-NET-1 reserved IP range, designed specifically for documentation and testing. That's why the expected log line contains that exact IP and port.

---

## Summary

| Piece | Purpose |
|---|---|
| `&bytes.Buffer{}` | Captures log output in memory so the test can read it back |
| `ReplaceAttr` hook | Pins the timestamp to a fixed value for deterministic output |
| `slog.TimeKey` | The constant string `"time"`, slog's built-in key for the timestamp |
| `groups []string` | Tracks which nested `WithGroup(...)` namespace an attribute lives in |
| `httptest.NewRequest` | Fabricates a fake HTTP request object, no network involved |
| `httptest.NewRecorder` | Fake `ResponseWriter` that buffers status, headers, and body in memory |
| `192.0.2.1:1234` | Default `RemoteAddr` set by `httptest.NewRequest`, from the TEST-NET-1 range |