package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
)

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

func initializeLogger(logFile string) (*log.Logger, closeFunc, error) {
	var logger *log.Logger

	if logFile == "" {
		logger = log.New(os.Stderr, "", log.LstdFlags)

		return logger, nil, nil
	}

	file, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)

	bufferedFile := bufio.NewWriterSize(file, 8192)

	if err != nil {
		return nil, nil, fmt.Errorf("failed to open log file: %w", err)
	}

	multiWriter := io.MultiWriter(os.Stderr, bufferedFile)

	logger = log.New(multiWriter, "", log.LstdFlags)

	return logger, func() error {
		err := bufferedFile.Flush()

		if err != nil {
			fmt.Errorf("failed to flush the buffer: %w", err)
		}

		err = file.Close()

		if err != nil {
			fmt.Errorf("failed to close log file: %w", err)
		}

		return nil

	}, nil
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logger, closeFunc, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))

	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}

	st, err := store.New(dataDir, logger)

	if err != nil {
		logger.Printf("failed to create store: %v", err)
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

	logger.Println("Linko is shutting down")

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Printf("failed to shutdown server: %v", err)
		return 1
	}
	if serverErr != nil {
		logger.Printf("server error: %v", serverErr)
		return 1
	}

	return 0
}
