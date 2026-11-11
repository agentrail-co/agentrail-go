package agentrail_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	agentrail "github.com/agentrail-co/agentrail-go"
)

// Fails if locations disappear at export, leak between requests, or miss nested entries.
func TestIncomingLocationIsScopedAndBounded(t *testing.T) {
	r, sink := collector(t)
	o := options(sink.URL)
	location := agentrail.IPLocation{Country: "US", Region: "Washington", City: "Seattle", Source: agentrail.LocationDBIP}
	o.IncomingLocation = func(*http.Request) (agentrail.IPLocation, error) { return location, nil }
	c, err := agentrail.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	firstEntered, releaseFirst, firstDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/first" {
			close(firstEntered)
			<-releaseFirst
		}
		c.Milestone(q.Context(), "inside"+q.URL.Path, agentrail.MilestoneContext{Surface: agentrail.SurfaceCustom, Kind: "inside"})
		w.WriteHeader(http.StatusAccepted)
	}))
	go func() {
		defer close(firstDone)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://app.test/first", nil))
	}()
	<-firstEntered
	// Mutation is synchronized with the first callback's completion by firstEntered.
	location = agentrail.IPLocation{Country: "DE", City: strings.Repeat("B", 129), Source: agentrail.LocationVercel}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://app.test/second", nil))
	close(releaseFirst)
	<-firstDone
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ss := spans(r)
	if len(ss) != 4 {
		t.Fatalf("got %d spans, want two HTTP and two milestone entries", len(ss))
	}
	for _, span := range ss {
		path := attr(span.Attributes, "url.full").GetStringValue()
		first := strings.HasSuffix(path, "/first") || span.Name == "inside/first"
		want := []string{"DE", "", strings.Repeat("B", 128), "vercel"}
		if first {
			want = []string{"US", "Washington", "Seattle", "dbip"}
		}
		var got []string
		for _, field := range []string{"country", "region", "city", "source"} {
			got = append(got, attr(span.Attributes, "agentrail.client.location."+field).GetStringValue())
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s location = %v, want %v", span.Name, got, want)
		}
		if !first && attr(span.Attributes, "agentrail.client.location.region") != nil {
			t.Error("empty region was exported")
		}
	}
}

// Fails if unknown/invalid locations poison exports or hook failures replace application responses.
func TestIncomingLocationFailuresAndUnknown(t *testing.T) {
	for _, name := range []string{"unset", "unknown", "error", "panic", "country", "source"} {
		t.Run(name, func(t *testing.T) {
			r, sink := collector(t)
			o := options(sink.URL)
			var issues []agentrail.CaptureError
			o.OnError = func(_ context.Context, issue agentrail.CaptureError) { issues = append(issues, issue) }
			if name != "unset" {
				o.IncomingLocation = func(*http.Request) (agentrail.IPLocation, error) {
					switch name {
					case "error":
						return agentrail.IPLocation{}, errors.New("private lookup detail")
					case "panic":
						panic("private lookup detail")
					case "country":
						return agentrail.IPLocation{Country: "invalid"}, nil
					case "source":
						return agentrail.IPLocation{Country: "US", Source: "invalid"}, nil
					default:
						return agentrail.IPLocation{}, nil
					}
				}
			}
			c, err := agentrail.New(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := c.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			response := httptest.NewRecorder()
			c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })).ServeHTTP(response, httptest.NewRequest("GET", "http://app.test/", nil))
			if response.Code != http.StatusAccepted {
				t.Fatalf("response replaced: %d", response.Code)
			}
			if err := c.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantIssues := 1
			if name == "unset" || name == "unknown" {
				wantIssues = 0
			}
			if len(issues) != wantIssues {
				t.Fatalf("got issues %v, want %d", issues, wantIssues)
			}
			if wantIssues == 1 && issues[0].Code != "location_failed" {
				t.Fatalf("wrong issue: %v", issues)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			ss := spans(r)
			if len(ss) != 1 {
				t.Fatalf("got %d spans, want the HTTP entry", len(ss))
			}
			for _, a := range ss[0].Attributes {
				if strings.HasPrefix(a.Key, "agentrail.client.location.") {
					t.Errorf("unavailable location was exported: %s", a.Key)
				}
			}
		})
	}
}
