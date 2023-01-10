package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"sigsum.org/sigsum-go/pkg/types"
)

// TestPath checks that Path works for an endpoint (add-leaf)
func TestPath(t *testing.T) {
	testFun := func(_ context.Context, _ http.ResponseWriter, _ *http.Request) (int, error) {
		return 0, nil
	}
	for _, table := range []struct {
		description string
		prefix      string
		want        string
	}{
		{
			description: "no prefix",
			want:        "/add-leaf",
		},
		{
			description: "a prefix",
			prefix:      "test-prefix",
			want:        "/test-prefix/add-leaf",
		},
	} {
		h := Handler{Config{}, testFun, types.EndpointAddLeaf, http.MethodPost}
		if got, want := h.path(table.prefix), table.want; got != want {
			t.Errorf("got path %v but wanted %v", got, want)
		}
	}
}

// func TestServeHTTP(t *testing.T) {
// 	h.ServeHTTP(w http.ResponseWriter, r *http.Request)
// }

func TestValidMethod(t *testing.T) {
	badMethod := "badMethod"
	for _, h := range []Handler{
		{
			Endpoint: types.EndpointAddLeaf,
			Method:   http.MethodPost,
		},
		{
			Endpoint: types.EndpointGetNextTreeHead,
			Method:   http.MethodGet,
		},
	} {
		for _, method := range []string{
			// Known methods
			http.MethodGet,
			http.MethodPost,
			http.MethodHead,
			http.MethodPut,
			// Invalid method
			badMethod,
		} {
			url := h.Endpoint.Path("http://log.example.com", "fixme")
			req, err := http.NewRequest(method, url, nil)
			if err != nil {
				t.Fatalf("must create HTTP request: %v", err)
			}

			w := httptest.NewRecorder()
			valid := h.validMethod(w, req)
			if got, want := valid, h.Method == method; got != want {
				t.Errorf("%s %s: got %v but wanted %v: %v", method, url, got, want, err)
				continue
			}
			if valid {
				continue
			}

			if method == badMethod {
				if got, want := w.Code, http.StatusBadRequest; got != want {
					t.Errorf("%s %s: got status %d, wanted %d", method, url, got, want)
				}
				if _, ok := w.Header()["Allow"]; ok {
					t.Errorf("%s %s: got Allow header, wanted none", method, url)
				}
				continue
			}

			if got, want := w.Code, http.StatusMethodNotAllowed; got != want {
				t.Errorf("%s %s: got status %d, wanted %d", method, url, got, want)
			} else if methods, ok := w.Header()["Allow"]; !ok {
				t.Errorf("%s %s: got no allow header, expected one", method, url)
			} else if got, want := len(methods), 1; got != want {
				t.Errorf("%s %s: got %d allowed method(s), wanted %d", method, url, got, want)
			} else if got, want := methods[0], h.Method; got != want {
				t.Errorf("%s %s: got allowed method %s, wanted %s", method, url, got, want)
			}
		}
	}
}

// func TestHandle(t *testing.T) {
// 	h.handle(w http.ResponseWriter, r *http.Request)
// }
