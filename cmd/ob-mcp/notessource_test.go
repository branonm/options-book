package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenNotesFailureDoesNotStopTheServer(t *testing.T) {
	st := openNotesWith(context.Background(), "b", func(context.Context) (*http.Client, error) {
		return nil, errors.New("no credentials")
	})
	if _, err := st.List(context.Background(), false); err == nil || !strings.Contains(err.Error(), "no credentials") || !strings.Contains(err.Error(), "bucket b") {
		t.Errorf("list: %v", err)
	}
	if _, err := st.Write(context.Background(), "A.md", []byte("x"), 0); err == nil {
		t.Error("write on a failed store succeeded")
	}
}

func TestOpenNotesUnreadableBucketStillReturnsAStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "denied", 403) }))
	defer srv.Close()
	// The probe fails (403) but the store is still returned; its tools will report the error.
	st := openNotesWith(context.Background(), "b", func(context.Context) (*http.Client, error) {
		return &http.Client{Transport: rewrite{srv.URL}}, nil
	})
	if st == nil {
		t.Fatal("no store")
	}
	if _, err := st.List(context.Background(), false); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("list: %v", err)
	}
}

// rewrite sends every request to the test server.
type rewrite struct{ to string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	h := strings.TrimPrefix(r.to, "http://")
	u.Scheme, u.Host = "http", h
	req2 := req.Clone(req.Context())
	req2.URL = &u
	return http.DefaultTransport.RoundTrip(req2)
}
