package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/branonm/options-book/internal/notes"
)

// openNotes opens the notes bucket with the service's default credentials.
// Unlike CONFIG_BUCKET it never stops the server: if the credentials or the
// bucket are unusable it logs why and returns a store whose every call returns
// that error, so the notes tools say so and nothing else is affected.
func openNotes(ctx context.Context, bucket string) notes.Store {
	return openNotesWith(ctx, bucket, func(ctx context.Context) (*http.Client, error) {
		return google.DefaultClient(ctx, "https://www.googleapis.com/auth/devstorage.read_write")
	})
}

func openNotesWith(ctx context.Context, bucket string, client func(context.Context) (*http.Client, error)) notes.Store {
	log.Printf("notes: bucket %s", bucket)
	c, err := client(ctx)
	if err != nil {
		log.Printf("notes: %v", err)
		return failedNotes{fmt.Errorf("notes bucket %s: %w", bucket, err)}
	}
	st := notes.NewGCS(bucket, c)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := st.List(pctx, false); err != nil {
		log.Printf("notes: bucket %s is not readable: %v", bucket, err)
	}
	return st
}

// failedNotes is a notes.Store that returns err from every call.
type failedNotes struct{ err error }

func (f failedNotes) List(context.Context, bool) ([]notes.Note, error)         { return nil, f.err }
func (f failedNotes) Read(context.Context, string, int64) (*notes.Note, error) { return nil, f.err }
func (f failedNotes) Write(context.Context, string, []byte, int64) (*notes.Note, error) {
	return nil, f.err
}
