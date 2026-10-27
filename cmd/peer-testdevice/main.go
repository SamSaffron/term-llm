// peer-testdevice is a synthetic linked-device protocol test device.
// It is a standalone development tool, is not a production client, and is not
// registered with the term-llm Cobra command tree.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/samsaffron/term-llm/internal/linkeddevices/testdevice"
)

func main() {
	var (
		link        string
		statePath   string
		deviceName  string
		method      string
		requestPath string
		body        string
		timeout     time.Duration
	)
	flag.StringVar(&link, "link", "", "Full termllm://pair v2 link; omit to reconnect from -state")
	flag.StringVar(&statePath, "state", "", "Optional private synthetic-device identity/grant file")
	flag.StringVar(&deviceName, "name", "Synthetic test device", "Device name sent during first pairing")
	flag.StringVar(&method, "method", http.MethodGet, "HTTP method to send over the peer bridge")
	flag.StringVar(&requestPath, "path", "/", "Origin-relative HTTP request path")
	flag.StringVar(&body, "body", "", "Optional HTTP request body")
	flag.DurationVar(&timeout, "timeout", 30*time.Second, "Pairing, connection, and request timeout")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "SYNTHETIC TEST DEVICE — DEVELOPMENT/INTEROPERABILITY USE ONLY")
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if statePath == "" && link == "" {
		fatalf("either -link or -state is required")
	}
	if !strings.HasPrefix(requestPath, "/") {
		fatalf("-path must begin with /")
	}
	fmt.Fprintln(os.Stderr, "SYNTHETIC TEST DEVICE — not a production client")

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, timeout)
	defer cancel()
	configName := ""
	if link != "" {
		configName = deviceName
	}
	device, err := testdevice.New(testdevice.Config{
		DeviceName: configName,
		StatePath:  statePath,
		OnSAS: func(sas string) {
			fmt.Fprintf(os.Stderr, "Pairing SAS: %s (compare this with the host before approving)\n", sas)
		},
	})
	if err != nil {
		fatalf("%v", err)
	}
	if link != "" {
		if _, err := device.Pair(ctx, link); err != nil {
			fatalf("pair: %v", err)
		}
		fmt.Fprintln(os.Stderr, "Pairing approved and authenticated")
	}
	session, err := device.Connect(ctx)
	if err != nil {
		fatalf("connect: %v", err)
	}
	defer session.Close()

	req, err := http.NewRequestWithContext(ctx, method, "http://synthetic.invalid"+requestPath, strings.NewReader(body))
	if err != nil {
		fatalf("request: %v", err)
	}
	resp, err := session.HTTPClient().Do(req)
	if err != nil {
		fatalf("peer HTTP request: %v", err)
	}
	defer resp.Body.Close()
	fmt.Printf("HTTP/1.1 %s\n", resp.Status)
	if err := resp.Header.Write(os.Stdout); err != nil {
		fatalf("write response headers: %v", err)
	}
	fmt.Println()
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		fatalf("read response: %v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "peer-testdevice: "+format+"\n", args...)
	os.Exit(1)
}
