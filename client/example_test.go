package client_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/atvirokodosprendimai/ocr-routerv1/client"
)

// Example submits one file and prints the units the service produced.
//
// ⚠ These examples are compiled against the EXPORTED surface only, from outside
// the package. That is what makes them a check as well as documentation: rename
// or re-signature anything they touch and the package stops building. It is
// also the standing evidence for ADR-0011's central premise — that the exported
// surface is sufficient on its own, with no reach into an unexported helper.
func Example() {
	f, err := os.Open("invoice.pdf")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	// The context bounds the WHOLE operation, upload and wait together. Do not
	// put a Timeout on the http.Client instead: the SSE stream is held open for
	// the life of the job, and a client Timeout aborts it mid-wait.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	res, err := client.Submit(ctx,
		client.Config{
			RouterURL: "https://ocr.example.com",
			Token:     os.Getenv("OCR_TOKEN"),
		},
		client.Input{
			Filename: "invoice.pdf",
			Body:     f,
			Label:    "ocr",
		},
		nil, // no progress callback
	)
	if err != nil {
		log.Fatal(err)
	}

	for _, unit := range res.Units {
		fmt.Println(unit)
	}
}

// ExampleSubmit_progress shows the progress callback and the two errors worth
// branching on.
func ExampleSubmit_progress() {
	f, _ := os.Open("scan.png")
	defer f.Close()

	progress := func(stage client.Stage, detail string) {
		switch stage {
		case client.StageUploading:
			fmt.Println("uploading…")
		case client.StageWaiting:
			fmt.Println("queued as", detail)
		case client.StageCollecting:
			fmt.Println("collecting…")
		case client.StageDone:
			fmt.Println("done")
		}
	}

	res, err := client.Submit(context.Background(),
		client.Config{
			RouterURL: "https://ocr.example.com",
			Token:     os.Getenv("OCR_TOKEN"),
			// Supply your own client for a proxy or custom TLS. It must NOT
			// carry a Timeout — bound the operation with the context instead.
			HTTP: &http.Client{},
		},
		client.Input{Filename: "scan.png", Body: f, Label: "ocr"},
		progress,
	)

	// ⚠ BRANCH ON THE TYPED ERRORS. A job that ran and died is not the same as
	// a router that was briefly unreachable, and treating either as "no result"
	// is how a dead job becomes an empty file and silent data loss downstream.
	var failed *client.FailedError
	var retryable *client.RetryableError
	switch {
	case errors.As(err, &failed):
		fmt.Println("the job failed:", failed.Reason)
	case errors.As(err, &retryable):
		fmt.Println("worth trying again:", retryable)
	case err != nil:
		fmt.Println("fatal:", err)
	default:
		fmt.Println("units:", len(res.Units))
	}
}

// ExampleInput_params submits a job with no file at all — the crawler shape,
// where the service fetches its own input and the parameters become the
// worker subprocess's flags.
func ExampleInput_params() {
	res, err := client.Submit(context.Background(),
		client.Config{RouterURL: "https://ocr.example.com", Token: os.Getenv("OCR_TOKEN")},
		client.Input{
			Label:  "crawl",
			Params: map[string]string{"url": "https://example.com"},
		},
		nil,
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.JobID, len(res.Units))
}
