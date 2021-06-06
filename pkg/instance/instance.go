package stfe

import (
	"context"
	"crypto"
	"fmt"
	"net/http"
	"time"

	"github.com/golang/glog"
	"github.com/system-transparency/stfe/pkg/state"
	"github.com/system-transparency/stfe/pkg/trillian"
	"github.com/system-transparency/stfe/pkg/types"
)

// Config is a collection of log parameters
type Config struct {
	LogID    string        // H(public key), then hex-encoded
	TreeID   int64         // Merkle tree identifier used by Trillian
	Prefix   string        // The portion between base URL and st/v0 (may be "")
	MaxRange int64         // Maximum number of leaves per get-leaves request
	Deadline time.Duration // Deadline used for gRPC requests
	Interval time.Duration // Cosigning frequency

	// Witnesses map trusted witness identifiers to public verification keys
	Witnesses map[[types.HashSize]byte][types.VerificationKeySize]byte
}

// Instance is an instance of the log's front-end
type Instance struct {
	Config                      // configuration parameters
	Client   trillian.Client    // provides access to the Trillian backend
	Signer   crypto.Signer      // provides access to Ed25519 private key
	Stateman state.StateManager // coordinates access to (co)signed tree heads
}

// Handler implements the http.Handler interface, and contains a reference
// to an STFE server instance as well as a function that uses it.
type Handler struct {
	Instance *Instance
	Endpoint types.Endpoint
	Method   string
	Handler  func(context.Context, *Instance, http.ResponseWriter, *http.Request) (int, error)
}

// Handlers returns a list of STFE handlers
func (i *Instance) Handlers() []Handler {
	return []Handler{
		Handler{Instance: i, Handler: addLeaf, Endpoint: types.EndpointAddLeaf, Method: http.MethodPost},
		Handler{Instance: i, Handler: addCosignature, Endpoint: types.EndpointAddCosignature, Method: http.MethodPost},
		Handler{Instance: i, Handler: getTreeHeadLatest, Endpoint: types.EndpointGetTreeHeadLatest, Method: http.MethodGet},
		Handler{Instance: i, Handler: getTreeHeadToSign, Endpoint: types.EndpointGetTreeHeadToSign, Method: http.MethodGet},
		Handler{Instance: i, Handler: getTreeHeadCosigned, Endpoint: types.EndpointGetTreeHeadCosigned, Method: http.MethodGet},
		Handler{Instance: i, Handler: getConsistencyProof, Endpoint: types.EndpointGetConsistencyProof, Method: http.MethodPost},
		Handler{Instance: i, Handler: getInclusionProof, Endpoint: types.EndpointGetProofByHash, Method: http.MethodPost},
		Handler{Instance: i, Handler: getLeaves, Endpoint: types.EndpointGetLeaves, Method: http.MethodPost},
	}
}

// Path returns a path that should be configured for this handler
func (h Handler) Path() string {
	return h.Endpoint.Path(h.Instance.Prefix, "st", "v0")
}

// ServeHTTP is part of the http.Handler interface
func (a Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// export prometheus metrics
	var now time.Time = time.Now()
	var statusCode int
	defer func() {
		rspcnt.Inc(a.Instance.LogID, string(a.Endpoint), fmt.Sprintf("%d", statusCode))
		latency.Observe(time.Now().Sub(now).Seconds(), a.Instance.LogID, string(a.Endpoint), fmt.Sprintf("%d", statusCode))
	}()
	reqcnt.Inc(a.Instance.LogID, string(a.Endpoint))

	ctx, cancel := context.WithDeadline(r.Context(), now.Add(a.Instance.Deadline))
	defer cancel()

	if r.Method != a.Method {
		glog.Warningf("%s/%s: got HTTP %s, wanted HTTP %s", a.Instance.Prefix, string(a.Endpoint), r.Method, a.Method)
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}

	statusCode, err := a.Handler(ctx, a.Instance, w, r)
	if err != nil {
		glog.Warningf("handler error %s/%s: %v", a.Instance.Prefix, a.Endpoint, err)
		http.Error(w, fmt.Sprintf("%s%s%s%s", "Error", types.Delim, err.Error(), types.EOL), statusCode)
	}
}
