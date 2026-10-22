package main

import (
	_ "embed"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/mitstack-eu/operator-kit/v3/handler"
	"github.com/mitstack-eu/operator-kit/v3/webhook"
)

//go:embed manifest.yaml
var manifestYAML []byte

const (
	defaultListenAddr = ":8080"
	logBufferLines    = 500
	readHeaderTimeout = 10 * time.Second
)

// handlerConfig serves the block through its block-level handler, with one observer per declared resource. A verb
// observes each resource once and answers, since a call held open polling would spend the platform's verb timeout;
// the settle check's validate and its re-drive carry the rest.
func handlerConfig(jwksURL string, logs *webhook.LogBuffer) handler.Config {
	return handler.Config{
		Resource:   &blockResource{},
		Manifest:   manifestYAML,
		Observers:  observers(),
		JWKSURL:    jwksURL,
		Logs:       logs,
		VerbBudget: 1,
	}
}

func main() {
	jwksURL := os.Getenv("MITSTACK_JWKS_URL")
	if jwksURL == "" {
		log.Fatal("MITSTACK_JWKS_URL is required: the trust boundary resolves mitstack's signing key from it")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = defaultListenAddr
	}

	// /logs serves only what passes through the standard logger.
	logs := webhook.NewLogBuffer(logBufferLines)
	log.SetOutput(io.MultiWriter(os.Stderr, logs.Writer()))

	h, err := handler.New(handlerConfig(jwksURL, logs))
	if err != nil {
		log.Fatalf("building the lifecycle handler: %v", err)
	}

	mux := http.NewServeMux()
	// The platform's secret-leak and teardown gates select an operator on the bare "/hook" literal.
	mux.HandleFunc("/hook", h.Serve)
	mux.HandleFunc("/logs", h.Serve)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: readHeaderTimeout}
	log.Printf("operator listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serving: %v", err)
	}
}
