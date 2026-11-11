package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLegacyMigrateCommandDoesNotConnectOrApplyDDL(t *testing.T) {
	previous := os.Args
	os.Args = []string{"dashboard", "migrate"}
	t.Cleanup(func() { os.Args = previous })
	t.Setenv("DATABASE_URL", "postgres://invalid:invalid@127.0.0.1:1/invalid")
	err := run()
	if err == nil || !strings.Contains(err.Error(), "managed by Liquibase") {
		t.Fatalf("expected Liquibase guidance before any database connection, got %v", err)
	}
}

func TestCollectCommandPointsToCollector(t *testing.T) {
	previous := os.Args
	os.Args = []string{"dashboard", "collect"}
	t.Cleanup(func() { os.Args = previous })
	t.Setenv("DATABASE_URL", "postgres://invalid:invalid@127.0.0.1:1/invalid")
	err := run()
	if err == nil || !strings.Contains(err.Error(), "collector executable") {
		t.Fatalf("expected collector guidance, got %v", err)
	}
}

func TestDemoBind(t *testing.T) {
	for _, tc := range []struct {
		addr   string
		demo   bool
		user   string
		allow  bool
		wantOK bool
	}{
		{"127.0.0.1:9000", true, "", false, true},
		{"[::1]:9000", true, "", false, true},
		{"localhost:9000", true, "", false, true},
		{"0.0.0.0:8080", true, "", false, false},
		{":9000", true, "", false, false},
		{"0.0.0.0:8080", true, "", true, true},
		{"0.0.0.0:8080", true, "user", false, true},
		{"0.0.0.0:8080", false, "user", false, true},
	} {
		if got := validateDemoBind(tc.addr, tc.demo, tc.user, tc.allow); (got == nil) != tc.wantOK {
			t.Errorf("%+v: %v", tc, got)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer s.Close()
	_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	if err := healthcheck("0.0.0.0:" + port); err != nil {
		t.Fatal(err)
	}
	status = http.StatusServiceUnavailable
	if err := healthcheck(s.Listener.Addr().String()); err == nil {
		t.Fatal("unhealthy endpoint accepted")
	}
	if err := healthcheck("invalid"); err == nil {
		t.Fatal("invalid address accepted")
	}
}

func TestServeDrainsActiveRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	listener := make(chan net.Listener, 1)
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}), BaseContext: func(l net.Listener) context.Context { listener <- l; return context.Background() }}
	result := make(chan error, 1)
	go func() { result <- serve(ctx, server) }()
	var addr string
	select {
	case l := <-listener:
		addr = l.Addr().String()
	case <-time.After(3 * time.Second):
		t.Fatal("server did not start")
	}
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, err := http.Get("http://" + addr)
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-result:
		t.Fatalf("server returned before active request completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	<-requestDone
}
