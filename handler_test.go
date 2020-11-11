package stfe

import (
	"bytes"
	"context"
	"crypto"
	"fmt"
	"strings"
	"testing"
	"time"

	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/golang/mock/gomock"
	"github.com/google/certificate-transparency-go/trillian/mockclient"
	cttestdata "github.com/google/certificate-transparency-go/trillian/testdata"
	"github.com/google/trillian"
	"github.com/google/trillian/types"
	"github.com/system-transparency/stfe/server/testdata"
	"github.com/system-transparency/stfe/x509util"
)

type testHandler struct {
	mockCtrl *gomock.Controller
	client   *mockclient.MockTrillianLogClient
	instance *Instance
}

func newTestHandler(t *testing.T, signer crypto.Signer) *testHandler {
	anchorList, err := x509util.NewCertificateList(testdata.PemAnchors)
	if err != nil {
		t.Fatalf("failed parsing trust anchors: %v", err)
	}
	ctrl := gomock.NewController(t)
	client := mockclient.NewMockTrillianLogClient(ctrl)
	return &testHandler{
		mockCtrl: ctrl,
		client:   client,
		instance: &Instance{
			Deadline: time.Second * 10, // TODO: fix me?
			Client:   client,
			LogParameters: &LogParameters{
				LogId:      make([]byte, 32),
				TreeId:     0,
				Prefix:     "/test",
				MaxRange:   3,
				MaxChain:   3,
				AnchorPool: x509util.NewCertPool(anchorList),
				AnchorList: anchorList,
				KeyUsage:   []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
				Signer:     signer,
				HashType:   crypto.SHA256,
			},
		},
	}
}

func (th *testHandler) getHandlers(t *testing.T) map[string]handler {
	return map[string]handler{
		"get-sth":               handler{instance: th.instance, handler: getSth, endpoint: "get-sth", method: http.MethodGet},
		"get-consistency-proof": handler{instance: th.instance, handler: getConsistencyProof, endpoint: "get-consistency-proof", method: http.MethodGet},
		"get-proof-by-hash":     handler{instance: th.instance, handler: getProofByHash, endpoint: "get-proof-by-hash", method: http.MethodGet},
		"get-anchors":           handler{instance: th.instance, handler: getAnchors, endpoint: "get-anchors", method: http.MethodGet},
		"get-entries":           handler{instance: th.instance, handler: getEntries, endpoint: "get-entries", method: http.MethodGet},
	}
}

func (th *testHandler) getHandler(t *testing.T, endpoint string) handler {
	handler, ok := th.getHandlers(t)[endpoint]
	if !ok {
		t.Fatalf("no such get endpoint: %s", endpoint)
	}
	return handler
}

func (th *testHandler) postHandlers(t *testing.T) map[string]handler {
	return map[string]handler{
		"add-entry": handler{instance: th.instance, handler: addEntry, endpoint: "add-entry", method: http.MethodPost},
	}
}

func (th *testHandler) postHandler(t *testing.T, endpoint string) handler {
	handler, ok := th.postHandlers(t)[endpoint]
	if !ok {
		t.Fatalf("no such post endpoint: %s", endpoint)
	}
	return handler
}

// TestGetHandlersRejectPost checks that all get handlers reject post requests
func TestGetHandlersRejectPost(t *testing.T) {
	th := newTestHandler(t, nil)
	defer th.mockCtrl.Finish()

	for endpoint, handler := range th.getHandlers(t) {
		t.Run(endpoint, func(t *testing.T) {
			s := httptest.NewServer(handler)
			defer s.Close()

			url := s.URL + strings.Join([]string{th.instance.LogParameters.Prefix, endpoint}, "/")
			if rsp, err := http.Post(url, "application/json", nil); err != nil {
				t.Fatalf("http.Post(%s)=(_,%q), want (_,nil)", url, err)
			} else if rsp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("http.Post(%s)=(%d,nil), want (%d, nil)", url, rsp.StatusCode, http.StatusMethodNotAllowed)
			}
		})
	}
}

// TestPostHandlersRejectGet checks that all post handlers reject get requests
func TestPostHandlersRejectGet(t *testing.T) {
	th := newTestHandler(t, nil)
	defer th.mockCtrl.Finish()

	for endpoint, handler := range th.postHandlers(t) {
		t.Run(endpoint, func(t *testing.T) {
			s := httptest.NewServer(handler)
			defer s.Close()

			url := s.URL + strings.Join([]string{th.instance.LogParameters.Prefix, endpoint}, "/")
			if rsp, err := http.Get(url); err != nil {
				t.Fatalf("http.Get(%s)=(_,%q), want (_,nil)", url, err)
			} else if rsp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("http.Get(%s)=(%d,nil), want (%d, nil)", url, rsp.StatusCode, http.StatusMethodNotAllowed)
			}
		})
	}
}

func TestGetSth(t *testing.T) {
	for _, table := range []struct {
		description string
		trsp        *trillian.GetLatestSignedLogRootResponse
		terr        error
		wantCode    int
		wantErrText string
		signer      crypto.Signer
	}{
		{
			description: "empty trillian response",
			terr:        fmt.Errorf("back-end failure"),
			wantCode:    http.StatusInternalServerError,
			wantErrText: http.StatusText(http.StatusInternalServerError) + "\n",
		},
		{
			description: "incomplete trillian response: nil response",
			wantCode:    http.StatusInternalServerError,
			wantErrText: http.StatusText(http.StatusInternalServerError) + "\n",
		},
		{
			description: "incomplete trillian response: no signed log root",
			trsp:        &trillian.GetLatestSignedLogRootResponse{SignedLogRoot: nil},
			wantCode:    http.StatusInternalServerError,
			wantErrText: http.StatusText(http.StatusInternalServerError) + "\n",
		},
		{
			description: "incomplete trillian response: truncated log root",
			trsp:        makeTruncatedSignedLogRoot(t),
			wantCode:    http.StatusInternalServerError,
			wantErrText: http.StatusText(http.StatusInternalServerError) + "\n",
		},
		{
			description: "incomplete trillian response: invalid root hash size",
			trsp:        makeSignedLogRoot(t, 0, 0, make([]byte, 31)),
			wantCode:    http.StatusInternalServerError,
			wantErrText: http.StatusText(http.StatusInternalServerError) + "\n",
		},
		{
			description: "marshal failure: no signature",
			trsp:        makeSignedLogRoot(t, 0, 0, make([]byte, 32)),
			wantCode:    http.StatusInternalServerError,
			wantErrText: http.StatusText(http.StatusInternalServerError) + "\n",
			signer:      cttestdata.NewSignerWithFixedSig(nil, make([]byte, 0)),
		},
		{
			description: "signature failure",
			trsp:        makeSignedLogRoot(t, 0, 0, make([]byte, 32)),
			wantCode:    http.StatusInternalServerError,
			wantErrText: http.StatusText(http.StatusInternalServerError) + "\n",
			signer:      cttestdata.NewSignerWithErr(nil, fmt.Errorf("signing failed")),
		},
		{
			description: "valid request and response",
			trsp:        makeSignedLogRoot(t, 0, 0, make([]byte, 32)),
			wantCode:    http.StatusOK,
			signer:      cttestdata.NewSignerWithFixedSig(nil, make([]byte, 32)),
		},
	} {
		func() { // run deferred functions at the end of each iteration
			th := newTestHandler(t, table.signer)
			defer th.mockCtrl.Finish()

			url := "http://example.com" + th.instance.LogParameters.Prefix + "/get-sth"
			req, err := http.NewRequest("GET", url, nil)
			if err != nil {
				t.Fatalf("failed creating http request: %v", err)
			}

			w := httptest.NewRecorder()
			th.client.EXPECT().GetLatestSignedLogRoot(newDeadlineMatcher(), gomock.Any()).Return(table.trsp, table.terr)
			th.getHandler(t, "get-sth").ServeHTTP(w, req)
			if w.Code != table.wantCode {
				t.Errorf("GET(%s)=%d, want http status code %d", url, w.Code, table.wantCode)
			}

			body := w.Body.String()
			if w.Code != http.StatusOK {
				if body != table.wantErrText {
					t.Errorf("GET(%s)=%q, want text %q", url, body, table.wantErrText)
				}
				return
			}

			// status code is http.StatusOK, check response
			var data []byte
			if err := json.Unmarshal([]byte(body), &data); err != nil {
				t.Errorf("failed unmarshaling json: %v, wanted ok", err)
				return
			}
			var item StItem
			if err := item.Unmarshal(data); err != nil {
				t.Errorf("failed unmarshaling StItem: %v, wanted ok", err)
				return
			}
			if item.Format != StFormatSignedTreeHeadV1 {
				t.Errorf("invalid StFormat: got %v, want %v", item.Format, StFormatSignedTreeHeadV1)
			}
			sth := item.SignedTreeHeadV1
			if !bytes.Equal(sth.LogId, th.instance.LogParameters.LogId) {
				t.Errorf("want log id %X, got %X", sth.LogId, th.instance.LogParameters.LogId)
			}
			if !bytes.Equal(sth.Signature, make([]byte, 32)) {
				t.Errorf("want signature %X, got %X", sth.Signature, make([]byte, 32))
			}
			if sth.TreeHead.TreeSize != 0 {
				t.Errorf("want tree size %d, got %d", 0, sth.TreeHead.TreeSize)
			}
			if sth.TreeHead.Timestamp != 0 {
				t.Errorf("want timestamp %d, got %d", 0, sth.TreeHead.Timestamp)
			}
			if !bytes.Equal(sth.TreeHead.RootHash.Data, make([]byte, 32)) {
				t.Errorf("want root hash %X, got %X", make([]byte, 32), sth.TreeHead.RootHash)
			}
			if len(sth.TreeHead.Extension) != 0 {
				t.Errorf("want no extensions, got %v", sth.TreeHead.Extension)
			}
		}()
	}
}

func makeSignedLogRoot(t *testing.T, timestamp, size uint64, hash []byte) *trillian.GetLatestSignedLogRootResponse {
	return &trillian.GetLatestSignedLogRootResponse{
		SignedLogRoot: mustMarshalRoot(t, &types.LogRootV1{
			TimestampNanos: timestamp,
			TreeSize:       size,
			RootHash:       hash,
		}),
	}
}

func makeTruncatedSignedLogRoot(t *testing.T) *trillian.GetLatestSignedLogRootResponse {
	slrr := makeSignedLogRoot(t, 0, 0, make([]byte, 32))
	slrr.SignedLogRoot.LogRoot = slrr.SignedLogRoot.LogRoot[1:]
	return slrr
}

func mustMarshalRoot(t *testing.T, lr *types.LogRootV1) *trillian.SignedLogRoot {
	rootBytes, err := lr.MarshalBinary()
	if err != nil {
		t.Fatalf("failed to marshal root in test: %v", err)
	}
	return &trillian.SignedLogRoot{LogRoot: rootBytes}
}

// deadlineMatcher implements gomock.Matcher, such that an error is detected if
// there is no context.Context deadline set
type deadlineMatcher struct{}

// newDeadlineMatcher returns a new deadlineMatcher
func newDeadlineMatcher() gomock.Matcher {
	return &deadlineMatcher{}
}

// Matches returns true if the passed interface is a context with a deadline
func (dm *deadlineMatcher) Matches(i interface{}) bool {
	ctx, ok := i.(context.Context)
	if !ok {
		return false
	}
	_, ok = ctx.Deadline()
	return ok
}

// String is needed to implement gomock.Matcher
func (dm *deadlineMatcher) String() string {
	return fmt.Sprintf("deadlineMatcher{}")
}
