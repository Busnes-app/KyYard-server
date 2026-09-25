package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestLocalDockerLoop(t *testing.T) {
	for _, terminal := range []error{nil, fmt.Errorf("initialization: %w", store.ErrForbidden)} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		done := make(chan struct{})
		calls := 0
		localDockerLoop(ctx, func(context.Context) error {
			calls++
			if calls == 1 {
				return errors.New("temporary startup failure")
			}
			return terminal
		}, done)
		cancel()
		if calls != 2 {
			t.Fatalf("attempts = %d, want 2", calls)
		}
		select {
		case <-done:
		default:
			t.Fatal("supervisor did not signal completion")
		}
	}
	t.Run("cancel_backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		failed := make(chan struct{})
		go localDockerLoop(ctx, func(context.Context) error {
			close(failed)
			return errors.New("socket not ready")
		}, done)
		<-failed
		cancel()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("backoff delayed shutdown")
		}
	})
}
