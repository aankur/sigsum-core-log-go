package instance

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"git.sigsum.org/sigsum-lib-go/pkg/types"
	mocksDB "git.sigsum.org/sigsum-log-go/pkg/db/mocks"
	mocksDNS "git.sigsum.org/sigsum-log-go/pkg/dns/mocks"
	mocksState "git.sigsum.org/sigsum-log-go/pkg/state/mocks"
	"github.com/golang/mock/gomock"
)

var (
	testWitVK  = types.PublicKey{}
	testConfig = Config{
		LogID:      fmt.Sprintf("%x", types.HashFn([]byte("logid"))[:]),
		TreeID:     0,
		Prefix:     "testonly",
		MaxRange:   3,
		Deadline:   10,
		Interval:   10,
		ShardStart: 10,
		Witnesses: map[types.Hash]types.PublicKey{
			*types.HashFn(testWitVK[:]): testWitVK,
		},
	}
	testSTH = &types.SignedTreeHead{
		TreeHead: types.TreeHead{
			Timestamp: 0,
			TreeSize:  0,
			RootHash:  *types.HashFn([]byte("root hash")),
		},
		Signature: types.Signature{},
	}
	testCTH = &types.CosignedTreeHead{
		SignedTreeHead: *testSTH,
		Cosignature:    []types.Signature{types.Signature{}},
		KeyHash:        []types.Hash{types.Hash{}},
	}
)

// TestHandlers check that the expected handlers are configured
func TestHandlers(t *testing.T) {
	endpoints := map[types.Endpoint]bool{
		types.EndpointAddLeaf:             false,
		types.EndpointAddCosignature:      false,
		types.EndpointGetTreeHeadToSign:   false,
		types.EndpointGetTreeHeadCosigned: false,
		types.EndpointGetConsistencyProof: false,
		types.EndpointGetInclusionProof:   false,
		types.EndpointGetLeaves:           false,
		types.Endpoint("get-checkpoint"):  false,
	}
	i := &Instance{
		Config: testConfig,
	}
	for _, handler := range i.Handlers() {
		if _, ok := endpoints[handler.Endpoint]; !ok {
			t.Errorf("got unexpected endpoint: %s", handler.Endpoint)
		}
		endpoints[handler.Endpoint] = true
	}
	for endpoint, ok := range endpoints {
		if !ok {
			t.Errorf("endpoint %s is not configured", endpoint)
		}
	}
}

// TestServeHTTP checks that invalid HTTP methods are rejected
func TestServeHTTP(t *testing.T) {
	i := &Instance{
		Config: testConfig,
	}
	for _, handler := range i.Handlers() {
		// Prepare invalid HTTP request
		method := http.MethodPost
		if method == handler.Method {
			method = http.MethodGet
		}
		url := handler.Endpoint.Path("http://example.com", i.Prefix)
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			t.Fatalf("must create HTTP request: %v", err)
		}
		w := httptest.NewRecorder()

		// Check that it is rejected
		handler.ServeHTTP(w, req)
		if got, want := w.Code, http.StatusMethodNotAllowed; got != want {
			t.Errorf("got HTTP code %v but wanted %v for endpoint %q", got, want, handler.Endpoint)
		}
	}
}

// TestPath checks that Path works for an endpoint (add-leaf)
func TestPath(t *testing.T) {
	for _, table := range []struct {
		description string
		prefix      string
		want        string
	}{
		{
			description: "no prefix",
			want:        "/sigsum/v0/add-leaf",
		},
		{
			description: "a prefix",
			prefix:      "test-prefix",
			want:        "/test-prefix/sigsum/v0/add-leaf",
		},
	} {
		instance := &Instance{
			Config: Config{
				Prefix: table.prefix,
			},
		}
		handler := Handler{
			Instance: instance,
			Handler:  addLeaf,
			Endpoint: types.EndpointAddLeaf,
			Method:   http.MethodPost,
		}
		if got, want := handler.Path(), table.want; got != want {
			t.Errorf("got path %v but wanted %v", got, want)
		}
	}
}

func TestAddLeaf(t *testing.T) {
	for _, table := range []struct {
		description    string
		ascii          io.Reader // buffer used to populate HTTP request
		expectTrillian bool      // expect Trillian client code path
		errTrillian    error     // error from Trillian client
		expectDNS      bool      // expect DNS verifier code path
		errDNS         error     // error from DNS verifier
		wantCode       int       // HTTP status ok
	}{
		{
			description: "invalid: bad request (parser error)",
			ascii:       bytes.NewBufferString("key=value\n"),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: bad request (signature error)",
			ascii:       mustLeafBuffer(t, 10, types.Hash{}, false),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: bad request (shard hint is before shard start)",
			ascii:       mustLeafBuffer(t, 9, types.Hash{}, true),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: bad request (shard hint is after shard end)",
			ascii:       mustLeafBuffer(t, uint64(time.Now().Unix())+1024, types.Hash{}, true),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: failed verifying domain hint",
			ascii:       mustLeafBuffer(t, 10, types.Hash{}, true),
			expectDNS:   true,
			errDNS:      fmt.Errorf("something went wrong"),
			wantCode:    http.StatusBadRequest,
		},
		{
			description:    "invalid: backend failure",
			ascii:          mustLeafBuffer(t, 10, types.Hash{}, true),
			expectDNS:      true,
			expectTrillian: true,
			errTrillian:    fmt.Errorf("something went wrong"),
			wantCode:       http.StatusInternalServerError,
		},
		{
			description:    "valid",
			ascii:          mustLeafBuffer(t, 10, types.Hash{}, true),
			expectDNS:      true,
			expectTrillian: true,
			wantCode:       http.StatusOK,
		},
	} {
		// Run deferred functions at the end of each iteration
		func() {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			dns := mocksDNS.NewMockVerifier(ctrl)
			if table.expectDNS {
				dns.EXPECT().Verify(gomock.Any(), gomock.Any(), gomock.Any()).Return(table.errDNS)
			}
			client := mocksDB.NewMockClient(ctrl)
			if table.expectTrillian {
				client.EXPECT().AddLeaf(gomock.Any(), gomock.Any()).Return(table.errTrillian)
			}
			i := Instance{
				Config: testConfig,
				Client: client,
				DNS:    dns,
			}

			// Create HTTP request
			url := types.EndpointAddLeaf.Path("http://example.com", i.Prefix)
			req, err := http.NewRequest("POST", url, table.ascii)
			if err != nil {
				t.Fatalf("must create http request: %v", err)
			}

			// Run HTTP request
			w := httptest.NewRecorder()
			mustHandle(t, i, types.EndpointAddLeaf).ServeHTTP(w, req)
			if got, want := w.Code, table.wantCode; got != want {
				t.Errorf("got HTTP status code %v but wanted %v in test %q", got, want, table.description)
			}
		}()
	}
}

func TestAddCosignature(t *testing.T) {
	buf := func() io.Reader {
		return bytes.NewBufferString(fmt.Sprintf("%s=%x\n%s=%x\n",
			"cosignature", types.Signature{},
			"key_hash", *types.HashFn(testWitVK[:]),
		))
	}
	for _, table := range []struct {
		description string
		ascii       io.Reader // buffer used to populate HTTP request
		expect      bool      // set if a mock answer is expected
		err         error     // error from Trillian client
		wantCode    int       // HTTP status ok
	}{
		{
			description: "invalid: bad request (parser error)",
			ascii:       bytes.NewBufferString("key=value\n"),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: bad request (unknown witness)",
			ascii: bytes.NewBufferString(fmt.Sprintf("%s=%x\n%s=%x\n",
				"cosignature", types.Signature{},
				"key_hash", *types.HashFn(testWitVK[1:]),
			)),
			wantCode: http.StatusBadRequest,
		},
		{
			description: "invalid: backend failure",
			ascii:       buf(),
			expect:      true,
			err:         fmt.Errorf("something went wrong"),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "valid",
			ascii:       buf(),
			expect:      true,
			wantCode:    http.StatusOK,
		},
	} {
		// Run deferred functions at the end of each iteration
		func() {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			stateman := mocksState.NewMockStateManager(ctrl)
			if table.expect {
				stateman.EXPECT().AddCosignature(gomock.Any(), gomock.Any(), gomock.Any()).Return(table.err)
			}
			i := Instance{
				Config:   testConfig,
				Stateman: stateman,
			}

			// Create HTTP request
			url := types.EndpointAddCosignature.Path("http://example.com", i.Prefix)
			req, err := http.NewRequest("POST", url, table.ascii)
			if err != nil {
				t.Fatalf("must create http request: %v", err)
			}

			// Run HTTP request
			w := httptest.NewRecorder()
			mustHandle(t, i, types.EndpointAddCosignature).ServeHTTP(w, req)
			if got, want := w.Code, table.wantCode; got != want {
				t.Errorf("got HTTP status code %v but wanted %v in test %q", got, want, table.description)
			}
		}()
	}
}

func TestGetTreeToSign(t *testing.T) {
	for _, table := range []struct {
		description string
		expect      bool                  // set if a mock answer is expected
		rsp         *types.SignedTreeHead // signed tree head from Trillian client
		err         error                 // error from Trillian client
		wantCode    int                   // HTTP status ok
	}{
		{
			description: "invalid: backend failure",
			expect:      true,
			err:         fmt.Errorf("something went wrong"),
			wantCode:    http.StatusInternalServerError,
		},
		{
			description: "valid",
			expect:      true,
			rsp:         testSTH,
			wantCode:    http.StatusOK,
		},
	} {
		// Run deferred functions at the end of each iteration
		func() {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			stateman := mocksState.NewMockStateManager(ctrl)
			if table.expect {
				stateman.EXPECT().ToCosignTreeHead(gomock.Any()).Return(table.rsp, table.err)
			}
			i := Instance{
				Config:   testConfig,
				Stateman: stateman,
			}

			// Create HTTP request
			url := types.EndpointGetTreeHeadToSign.Path("http://example.com", i.Prefix)
			req, err := http.NewRequest("GET", url, nil)
			if err != nil {
				t.Fatalf("must create http request: %v", err)
			}

			// Run HTTP request
			w := httptest.NewRecorder()
			mustHandle(t, i, types.EndpointGetTreeHeadToSign).ServeHTTP(w, req)
			if got, want := w.Code, table.wantCode; got != want {
				t.Errorf("got HTTP status code %v but wanted %v in test %q", got, want, table.description)
			}
		}()
	}
}

func TestGetTreeCosigned(t *testing.T) {
	for _, table := range []struct {
		description string
		expect      bool                    // set if a mock answer is expected
		rsp         *types.CosignedTreeHead // cosigned tree head from Trillian client
		err         error                   // error from Trillian client
		wantCode    int                     // HTTP status ok
	}{
		{
			description: "invalid: backend failure",
			expect:      true,
			err:         fmt.Errorf("something went wrong"),
			wantCode:    http.StatusInternalServerError,
		},
		{
			description: "valid",
			expect:      true,
			rsp:         testCTH,
			wantCode:    http.StatusOK,
		},
	} {
		// Run deferred functions at the end of each iteration
		func() {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			stateman := mocksState.NewMockStateManager(ctrl)
			if table.expect {
				stateman.EXPECT().CosignedTreeHead(gomock.Any()).Return(table.rsp, table.err)
			}
			i := Instance{
				Config:   testConfig,
				Stateman: stateman,
			}

			// Create HTTP request
			url := types.EndpointGetTreeHeadCosigned.Path("http://example.com", i.Prefix)
			req, err := http.NewRequest("GET", url, nil)
			if err != nil {
				t.Fatalf("must create http request: %v", err)
			}

			// Run HTTP request
			w := httptest.NewRecorder()
			mustHandle(t, i, types.EndpointGetTreeHeadCosigned).ServeHTTP(w, req)
			if got, want := w.Code, table.wantCode; got != want {
				t.Errorf("got HTTP status code %v but wanted %v in test %q", got, want, table.description)
			}
		}()
	}
}

func TestGetConsistencyProof(t *testing.T) {
	buf := func(oldSize, newSize int) io.Reader {
		return bytes.NewBufferString(fmt.Sprintf("%s=%d\n%s=%d\n",
			"old_size", oldSize,
			"new_size", newSize,
		))
	}
	for _, table := range []struct {
		description string
		ascii       io.Reader               // buffer used to populate HTTP request
		expect      bool                    // set if a mock answer is expected
		rsp         *types.ConsistencyProof // consistency proof from Trillian client
		err         error                   // error from Trillian client
		wantCode    int                     // HTTP status ok
	}{
		{
			description: "invalid: bad request (parser error)",
			ascii:       bytes.NewBufferString("key=value\n"),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: bad request (OldSize is zero)",
			ascii:       buf(0, 1),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: bad request (OldSize > NewSize)",
			ascii:       buf(2, 1),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: backend failure",
			ascii:       buf(1, 2),
			expect:      true,
			err:         fmt.Errorf("something went wrong"),
			wantCode:    http.StatusInternalServerError,
		},
		{
			description: "valid",
			ascii:       buf(1, 2),
			expect:      true,
			rsp: &types.ConsistencyProof{
				OldSize: 1,
				NewSize: 2,
				Path: []types.Hash{
					*types.HashFn([]byte{}),
				},
			},
			wantCode: http.StatusOK,
		},
	} {
		// Run deferred functions at the end of each iteration
		func() {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			client := mocksDB.NewMockClient(ctrl)
			if table.expect {
				client.EXPECT().GetConsistencyProof(gomock.Any(), gomock.Any()).Return(table.rsp, table.err)
			}
			i := Instance{
				Config: testConfig,
				Client: client,
			}

			// Create HTTP request
			url := types.EndpointGetConsistencyProof.Path("http://example.com", i.Prefix)
			req, err := http.NewRequest("POST", url, table.ascii)
			if err != nil {
				t.Fatalf("must create http request: %v", err)
			}

			// Run HTTP request
			w := httptest.NewRecorder()
			mustHandle(t, i, types.EndpointGetConsistencyProof).ServeHTTP(w, req)
			if got, want := w.Code, table.wantCode; got != want {
				t.Errorf("got HTTP status code %v but wanted %v in test %q", got, want, table.description)
			}
		}()
	}
}

func TestGetInclusionProof(t *testing.T) {
	buf := func(hash *types.Hash, treeSize int) io.Reader {
		return bytes.NewBufferString(fmt.Sprintf("%s=%x\n%s=%d\n",
			"leaf_hash", hash[:],
			"tree_size", treeSize,
		))
	}
	for _, table := range []struct {
		description string
		ascii       io.Reader             // buffer used to populate HTTP request
		expect      bool                  // set if a mock answer is expected
		rsp         *types.InclusionProof // inclusion proof from Trillian client
		err         error                 // error from Trillian client
		wantCode    int                   // HTTP status ok
	}{
		{
			description: "invalid: bad request (parser error)",
			ascii:       bytes.NewBufferString("key=value\n"),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: bad request (no proof for tree size)",
			ascii:       buf(types.HashFn([]byte{}), 1),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: backend failure",
			ascii:       buf(types.HashFn([]byte{}), 2),
			expect:      true,
			err:         fmt.Errorf("something went wrong"),
			wantCode:    http.StatusInternalServerError,
		},
		{
			description: "valid",
			ascii:       buf(types.HashFn([]byte{}), 2),
			expect:      true,
			rsp: &types.InclusionProof{
				TreeSize:  2,
				LeafIndex: 0,
				Path: []types.Hash{
					*types.HashFn([]byte{}),
				},
			},
			wantCode: http.StatusOK,
		},
	} {
		// Run deferred functions at the end of each iteration
		func() {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			client := mocksDB.NewMockClient(ctrl)
			if table.expect {
				client.EXPECT().GetInclusionProof(gomock.Any(), gomock.Any()).Return(table.rsp, table.err)
			}
			i := Instance{
				Config: testConfig,
				Client: client,
			}

			// Create HTTP request
			url := types.EndpointGetInclusionProof.Path("http://example.com", i.Prefix)
			req, err := http.NewRequest("POST", url, table.ascii)
			if err != nil {
				t.Fatalf("must create http request: %v", err)
			}

			// Run HTTP request
			w := httptest.NewRecorder()
			mustHandle(t, i, types.EndpointGetInclusionProof).ServeHTTP(w, req)
			if got, want := w.Code, table.wantCode; got != want {
				t.Errorf("got HTTP status code %v but wanted %v in test %q", got, want, table.description)
			}
		}()
	}
}

func TestGetLeaves(t *testing.T) {
	buf := func(startSize, endSize int64) io.Reader {
		return bytes.NewBufferString(fmt.Sprintf("%s=%d\n%s=%d\n",
			"start_size", startSize,
			"end_size", endSize,
		))
	}
	for _, table := range []struct {
		description string
		ascii       io.Reader     // buffer used to populate HTTP request
		expect      bool          // set if a mock answer is expected
		rsp         *types.Leaves // list of leaves from Trillian client
		err         error         // error from Trillian client
		wantCode    int           // HTTP status ok
	}{
		{
			description: "invalid: bad request (parser error)",
			ascii:       bytes.NewBufferString("key=value\n"),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: bad request (StartSize > EndSize)",
			ascii:       buf(1, 0),
			wantCode:    http.StatusBadRequest,
		},
		{
			description: "invalid: backend failure",
			ascii:       buf(0, 0),
			expect:      true,
			err:         fmt.Errorf("something went wrong"),
			wantCode:    http.StatusInternalServerError,
		},
		{
			description: "valid: one more entry than the configured MaxRange",
			ascii:       buf(0, testConfig.MaxRange), // query will be pruned
			expect:      true,
			rsp: func() *types.Leaves {
				var list types.Leaves
				for i := int64(0); i < testConfig.MaxRange; i++ {
					list = append(list[:], types.Leaf{
						Statement: types.Statement{
							ShardHint: 0,
							Checksum:  types.Hash{},
						},
						Signature: types.Signature{},
						KeyHash:   types.Hash{},
					})
				}
				return &list
			}(),
			wantCode: http.StatusOK,
		},
	} {
		// Run deferred functions at the end of each iteration
		func() {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			client := mocksDB.NewMockClient(ctrl)
			if table.expect {
				client.EXPECT().GetLeaves(gomock.Any(), gomock.Any()).Return(table.rsp, table.err)
			}
			i := Instance{
				Config: testConfig,
				Client: client,
			}

			// Create HTTP request
			url := types.EndpointGetLeaves.Path("http://example.com", i.Prefix)
			req, err := http.NewRequest("POST", url, table.ascii)
			if err != nil {
				t.Fatalf("must create http request: %v", err)
			}

			// Run HTTP request
			w := httptest.NewRecorder()
			mustHandle(t, i, types.EndpointGetLeaves).ServeHTTP(w, req)
			if got, want := w.Code, table.wantCode; got != want {
				t.Errorf("got HTTP status code %v but wanted %v in test %q", got, want, table.description)
			}
			if w.Code != http.StatusOK {
				return
			}

			list := types.Leaves{}
			if err := list.FromASCII(w.Body); err != nil {
				t.Fatalf("must unmarshal leaf list: %v", err)
			}
			if got, want := &list, table.rsp; !reflect.DeepEqual(got, want) {
				t.Errorf("got leaf list\n\t%v\nbut wanted\n\t%v\nin test %q", got, want, table.description)
			}
		}()
	}
}

func mustHandle(t *testing.T, i Instance, e types.Endpoint) Handler {
	for _, handler := range i.Handlers() {
		if handler.Endpoint == e {
			return handler
		}
	}
	t.Fatalf("must handle endpoint: %v", e)
	return Handler{}
}

func mustLeafBuffer(t *testing.T, shardHint uint64, preimage types.Hash, wantSig bool) io.Reader {
	t.Helper()

	vk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("must generate ed25519 keys: %v", err)
	}
	msg := types.Statement{
		ShardHint: shardHint,
		Checksum:  *types.HashFn(preimage[:]),
	}
	sig := ed25519.Sign(sk, msg.ToBinary())
	if !wantSig {
		sig[0] += 1
	}
	return bytes.NewBufferString(fmt.Sprintf(
		"%s=%d\n"+"%s=%x\n"+"%s=%x\n"+"%s=%x\n"+"%s=%s\n",
		"shard_hint", shardHint,
		"preimage", preimage[:],
		"signature", sig,
		"verification_key", vk,
		"domain_hint", "example.com",
	))
}
