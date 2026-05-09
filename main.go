package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	pkgerr "github.com/pkg/errors"
)

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

type closeFunc func() error

type multiError interface {
	error
	Unwrap() []error
}

func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
		{Key: "message", Value: slog.StringValue(err.Error())},
	}
	attrs = append(attrs, linkoerr.Attrs(err)...)
	if stackErr, ok := errors.AsType[stackTracer](err); ok {
		attrs = append(attrs, slog.Attr{
			Key:   "stack_trace",
			Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
		})
	}
	return attrs
}

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	replaceAttr := func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == "error" {
			err, ok := a.Value.Any().(error)

			if !ok {
				return a
			}

			if stackErr, ok := errors.AsType[stackTracer](err); ok {
				return slog.GroupAttrs("error", slog.Attr{
					Key:   "message",
					Value: slog.StringValue(stackErr.Error()),
				}, slog.Attr{
					Key:   "stack_trace",
					Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
				})
			}

			if multiErr, ok := errors.AsType[multiError](err); ok {
				var errAttrs []slog.Attr
				for i, e := range multiErr.Unwrap() {
					errAttrs = append(errAttrs, slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), errorAttrs(e)...))
				}
				return slog.GroupAttrs("errors", errAttrs...)
			}

			return slog.GroupAttrs("error", errorAttrs(err)...)
		}
		return a
	}

	handlers := []slog.Handler{
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
		}),
	}
	closers := []closeFunc{}

	if logFile != "" {
		file, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open log file: %w", err)
		}
		bufferedFile := bufio.NewWriterSize(file, 8192)
		close := func() error {
			if err := bufferedFile.Flush(); err != nil {
				return fmt.Errorf("failed to flush log file: %w", err)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("failed to close log file: %w", err)
			}
			return nil
		}
		handlers = append(handlers, slog.NewJSONHandler(bufferedFile, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		}))
		closers = append(closers, close)
	}
	closer := func() error {
		var errs []error
		for _, close := range closers {
			if err := close(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	return slog.New(slog.NewMultiHandler(handlers...)), closer, nil
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logger, closeFunc, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))

	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}

	st, err := store.New(dataDir, logger)

	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}
	s := newServer(*st, httpPort, logger, cancel)

	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	defer func() {
		err := closeFunc()

		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to close the logger: %v\n", err)
		}
	}()

	logger.Debug("Linko is shutting down")

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		logger.Error(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}

	return 0
}
