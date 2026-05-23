// Package server hosts Lantern's Lotus-compatible JSON-RPC endpoint.
//
// Built on github.com/filecoin-project/go-jsonrpc, the same library Lotus
// uses. We register handlers under the `Filecoin.` namespace and expose
// them on `http://<addr>/rpc/v1` (for both POST and WebSocket upgrade),
// matching Lotus' shape so a Lotus / Curio / sptool client can connect
// with `FULLNODE_API_INFO="<token>:/ip4/<host>/tcp/<port>/http"`.

package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/go-jsonrpc/auth"
	gjwt "github.com/gbrlsnchs/jwt/v3"

	"github.com/Reiers/lantern/api"
)

// Config configures the RPC server.
type Config struct {
	// ListenAddress is the TCP listen address, e.g. "127.0.0.1:1234".
	ListenAddress string
	// DataDir is where token files (lantern auth tokens) live.
	DataDir string
	// JWTSecret is the symmetric secret for the JWT auth scheme. When
	// empty, the server generates one and persists it under DataDir.
	JWTSecret []byte
}

// Server wraps an http.Server bound to a go-jsonrpc dispatcher.
type Server struct {
	cfg        Config
	rpcServer  *jsonrpc.RPCServer
	httpServer *http.Server
	listener   net.Listener
	auth       *Auth
}

// New creates a server bound to `node` and starts listening. The caller
// must call Run() (blocking) or Start()/Stop() to actually serve.
func New(cfg Config, node api.FullNode) (*Server, error) {
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:1234"
	}
	if cfg.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		cfg.DataDir = filepath.Join(home, ".lantern")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}

	a, err := LoadOrInitAuth(cfg.DataDir, cfg.JWTSecret)
	if err != nil {
		return nil, fmt.Errorf("init auth: %w", err)
	}

	// Custom method-name formatter so the same handler struct can be
	// registered under both:
	//   - 'Filecoin' namespace as `Filecoin.MethodName` (Lotus-compat)
	//   - 'eth' namespace as `eth_methodName` (viem / synapse-sdk)
	// Lantern issue #26 (eth_* coverage): the FoC client stack speaks
	// `eth_*` exclusively, so we register the Eth* handlers in both
	// namespaces without duplicating the Go methods.
	rs := jsonrpc.NewServer(jsonrpc.WithServerMethodNameFormatter(lanternMethodNameFormatter))

	// Register the FullNode directly. Lotus uses a permission-decorated
	// proxy struct (FullNodeStruct) with auto-generated method shims; for
	// Lantern V1 we enforce perms at the HTTP middleware (Auth.handle) by
	// mapping method name prefixes to required scope.
	rs.Register("Filecoin", node)

	// Also register under the 'eth' namespace so synapse-sdk + viem +
	// any other web3 client can speak eth_* directly. The ethAPI
	// wrapper exposes ONLY the Ethereum-API methods (not the entire
	// FullNode surface) so `eth_chainHead` etc. don't accidentally
	// land on the wire just because the underlying handler has those
	// methods.
	rs.Register("eth", newEthAPI(node))

	// Also register the small 'net' namespace. go-ethereum's
	// ethclient.NetworkID() calls net_version (chain id as decimal).
	// SenderETH in upstream curio uses NetworkID() during tx build.
	rs.Register("net", newNetAPI(node))

	mux := http.NewServeMux()
	authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, perms, err := a.handle(r.Context(), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		// Parse method name from JSON-RPC body for perm-gating.
		// We only need to peek; preserve the body for downstream.
		if r.Method == http.MethodPost && r.Body != nil {
			buf, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(buf))
			var req struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(buf, &req)
			if req.Method != "" {
				required := methodPermission(req.Method)
				if required != "" && !hasPerm(perms, required) {
					jsonRPCError(w, fmt.Errorf("missing permission to invoke '%s' (need '%s')", req.Method, required))
					return
				}
			}
		}
		rs.ServeHTTP(w, r.WithContext(ctx))
	})
	mux.Handle("/rpc/v1", authHandler)
	mux.Handle("/rpc/v0", authHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok\n"))
	})

	ln, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.ListenAddress, err)
	}

	hs := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return &Server{
		cfg:        cfg,
		rpcServer:  rs,
		httpServer: hs,
		listener:   ln,
		auth:       a,
	}, nil
}

// Addr returns the actual listening address (useful when ListenAddress
// was ":0").
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Auth returns the auth state (so callers can mint additional tokens
// for the CLI subcommands).
func (s *Server) Auth() *Auth { return s.auth }

// FullNodeAPIInfo returns the Lotus-compatible `FULLNODE_API_INFO` string
// for the admin-scoped token, ready to be set into the environment.
//
// Format: "<token>:/ip4/<host>/tcp/<port>/http"
func (s *Server) FullNodeAPIInfo() (string, error) {
	tok := s.auth.Token(api.PermAdmin)
	if tok == "" {
		return "", errors.New("admin token not initialised")
	}
	host, port, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		return "", err
	}
	// Use /ip4/ when host is an IPv4 / loopback.
	maddr := fmt.Sprintf("/ip4/%s/tcp/%s/http", normaliseHost(host), port)
	return fmt.Sprintf("%s:%s", tok, maddr), nil
}

func normaliseHost(h string) string {
	if h == "" || h == "::" {
		return "127.0.0.1"
	}
	return h
}

// Start launches the HTTP server in a background goroutine.
func (s *Server) Start() error {
	go func() {
		if err := s.httpServer.Serve(s.listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "rpc server error: %v\n", err)
		}
	}()
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// ---------------- Auth ----------------

// Auth holds the JWT signing secret and pre-minted tokens for each
// permission level.
type Auth struct {
	secret []byte
	tokens map[auth.Permission]string
}

// authPayload is the body of an issued JWT.
//
// Embeds gjwt.Payload (RFC 7519 standard claims) so we can ship exp/iat/jti
// claims while staying wire-compatible with v1.2.1 tokens that only had the
// Allow field. omitempty on every standard claim means a legacy token with
// no exp deserializes cleanly and AuthVerify treats it as never-expiring
// (logged but accepted; see #7 acceptance criteria around grace period).
type authPayload struct {
	gjwt.Payload
	Allow []auth.Permission
}

// TTL for each issued JWT scope. Tighter on higher-trust scopes; admin
// rotates monthly, read survives a year. See #7.
var tokenTTLs = map[auth.Permission]time.Duration{
	api.PermRead:  365 * 24 * time.Hour,
	api.PermWrite: 180 * 24 * time.Hour,
	api.PermSign:  90 * 24 * time.Hour,
	api.PermAdmin: 30 * 24 * time.Hour,
}

// ttlFor picks the shortest TTL among the permissions a token claims.
// A multi-scope token expires when the most-sensitive scope demands it.
func ttlFor(perms []auth.Permission) time.Duration {
	var ttl time.Duration
	for _, p := range perms {
		candidate, ok := tokenTTLs[p]
		if !ok {
			continue
		}
		if ttl == 0 || candidate < ttl {
			ttl = candidate
		}
	}
	if ttl == 0 {
		// Unknown perm set: pick the strictest known TTL as a safe default.
		ttl = tokenTTLs[api.PermAdmin]
	}
	return ttl
}

// LoadOrInitAuth loads jwt secret + pre-minted tokens from `dataDir`, or
// initialises them if absent. If `seed` is non-empty it overrides any
// on-disk secret.
func LoadOrInitAuth(dataDir string, seed []byte) (*Auth, error) {
	secretFile := filepath.Join(dataDir, "jwt-secret")
	var secret []byte
	if len(seed) > 0 {
		secret = seed
		if err := os.WriteFile(secretFile, secret, 0o600); err != nil {
			return nil, err
		}
	} else if b, err := os.ReadFile(secretFile); err == nil {
		secret = b
	} else if os.IsNotExist(err) {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		if err := os.WriteFile(secretFile, secret, 0o600); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}

	a := &Auth{secret: secret, tokens: map[auth.Permission]string{}}

	// Issue tokens for each scope. Persist the admin token in `token`
	// (Lotus convention: ~/.lotus/token == admin scope).
	scopes := map[string][]auth.Permission{
		"token-read":  {api.PermRead},
		"token-write": {api.PermRead, api.PermWrite},
		"token-sign":  {api.PermRead, api.PermWrite, api.PermSign},
		"token":       api.AllPerms, // admin
	}
	for fname, perms := range scopes {
		tok, err := a.mint(perms)
		if err != nil {
			return nil, err
		}
		// Top permission wins for the cached map.
		for _, p := range perms {
			cur, ok := a.tokens[p]
			if !ok || strings.Count(tok, ".") >= strings.Count(cur, ".") {
				a.tokens[p] = tok
			}
		}
		// Persist a copy.
		_ = os.WriteFile(filepath.Join(dataDir, fname), []byte(tok), 0o600)
	}
	return a, nil
}

// mint signs a new JWT with the given permission set.
//
// Issued tokens carry RFC 7519 exp/iat/jti claims (#7). Wire shape
// remains compatible with v1.2.1 readers because the new fields are
// omitempty additions on the same JSON object.
func (a *Auth) mint(perms []auth.Permission) (string, error) {
	now := time.Now()
	ttl := ttlFor(perms)

	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", fmt.Errorf("mint: random jti: %w", err)
	}

	hs := gjwt.NewHS256(a.secret)
	payload := authPayload{
		Payload: gjwt.Payload{
			Issuer:         "lantern",
			IssuedAt:       gjwt.NumericDate(now),
			ExpirationTime: gjwt.NumericDate(now.Add(ttl)),
			JWTID:          hex.EncodeToString(jti[:]),
		},
		Allow: perms,
	}
	token, err := gjwt.Sign(payload, hs)
	if err != nil {
		return "", err
	}
	return string(token), nil
}

// Token returns the pre-minted token at the requested permission level.
func (a *Auth) Token(p auth.Permission) string { return a.tokens[p] }

// AuthNew issues a new JWT with the requested perms.
func (a *Auth) AuthNew(perms []auth.Permission) ([]byte, error) {
	tok, err := a.mint(perms)
	if err != nil {
		return nil, err
	}
	return []byte(tok), nil
}

// AuthVerify validates a JWT and returns its claimed permissions.
//
// Tokens issued post-#7 carry an `exp` claim; we enforce it strictly:
// expired tokens fail with a clear error so the operator knows to rotate.
// Legacy tokens (v1.2.1 and earlier) have no exp claim; we accept them
// with a warning-class log line so we don't break running deployments
// during the one-release grace window.
func (a *Auth) AuthVerify(token string) ([]auth.Permission, error) {
	var p authPayload
	// First pass: signature + structural verify with no claim validators.
	// We need to know whether ExpirationTime is set before we can decide
	// whether to enforce it (legacy-token grace path).
	if _, err := gjwt.Verify([]byte(token), gjwt.NewHS256(a.secret), &p); err != nil {
		return nil, err
	}
	if p.ExpirationTime == nil {
		// Legacy token from before #7 shipped. Accept for one grace release.
		logLegacyTokenOnce()
		return p.Allow, nil
	}
	if time.Now().After(p.ExpirationTime.Time) {
		return nil, fmt.Errorf("token expired (issued %s, expired %s); rotate with 'lantern auth rotate'",
			formatTokenTime(p.IssuedAt), formatTokenTime(p.ExpirationTime))
	}
	return p.Allow, nil
}

// legacyTokenWarned ensures we log the legacy-token warning at most once
// per daemon lifetime so we don't spam the operator.
var legacyTokenWarned bool

func logLegacyTokenOnce() {
	if legacyTokenWarned {
		return
	}
	legacyTokenWarned = true
	fmt.Fprintln(os.Stderr, "WARN: accepting legacy JWT token without exp claim. Rotate with 'lantern auth rotate' to issue tokens with expiry.")
}

// TokenInfo describes an issued JWT for the auth-list command (#7).
type TokenInfo struct {
	File     string            // basename under dataDir (e.g. "token", "token-sign")
	Perms    []auth.Permission // permission set granted by this token
	IssuedAt time.Time         // when this token was minted
	Expires  time.Time         // when this token stops being accepted
	JTI      string            // unique token id (rotation evidence)
	Legacy   bool              // true when the token has no exp claim
}

// Inspect parses a previously-issued token (signed by this Auth's secret)
// and returns descriptive info for the operator. Used by `lantern auth list`.
func (a *Auth) Inspect(token string) (TokenInfo, error) {
	var p authPayload
	if _, err := gjwt.Verify([]byte(token), gjwt.NewHS256(a.secret), &p); err != nil {
		return TokenInfo{}, err
	}
	info := TokenInfo{Perms: p.Allow, JTI: p.JWTID}
	if p.IssuedAt != nil {
		info.IssuedAt = p.IssuedAt.Time
	}
	if p.ExpirationTime != nil {
		info.Expires = p.ExpirationTime.Time
	} else {
		info.Legacy = true
	}
	return info, nil
}

// Rotate generates a fresh JWT secret and reissues all four scope tokens
// under dataDir, invalidating every previously-issued token (#7).
func (a *Auth) Rotate(dataDir string) error {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("rotate: new secret: %w", err)
	}
	secretFile := filepath.Join(dataDir, "jwt-secret")
	if err := os.WriteFile(secretFile, secret, 0o600); err != nil {
		return fmt.Errorf("rotate: write secret: %w", err)
	}
	a.secret = secret
	a.tokens = map[auth.Permission]string{}

	scopes := map[string][]auth.Permission{
		"token-read":  {api.PermRead},
		"token-write": {api.PermRead, api.PermWrite},
		"token-sign":  {api.PermRead, api.PermWrite, api.PermSign},
		"token":       api.AllPerms,
	}
	for fname, perms := range scopes {
		tok, err := a.mint(perms)
		if err != nil {
			return fmt.Errorf("rotate: mint %s: %w", fname, err)
		}
		for _, p := range perms {
			cur, ok := a.tokens[p]
			if !ok || strings.Count(tok, ".") >= strings.Count(cur, ".") {
				a.tokens[p] = tok
			}
		}
		if err := os.WriteFile(filepath.Join(dataDir, fname), []byte(tok), 0o600); err != nil {
			return fmt.Errorf("rotate: write %s: %w", fname, err)
		}
	}
	return nil
}

// formatTokenTime returns a human-readable timestamp for error messages.
func formatTokenTime(t *gjwt.Time) string {
	if t == nil {
		return "unknown"
	}
	return t.Time.Format(time.RFC3339)
}

// handle parses the Authorization header (`Bearer <jwt>`) and returns a
// context carrying the caller's perms plus the perm slice for downstream
// gating.
func (a *Auth) handle(ctx context.Context, r *http.Request) (context.Context, []auth.Permission, error) {
	authHdr := r.Header.Get("Authorization")
	if authHdr == "" {
		return auth.WithPerm(ctx, api.DefaultPerms), api.DefaultPerms, nil
	}
	if !strings.HasPrefix(authHdr, "Bearer ") {
		return nil, nil, errors.New("malformed Authorization header")
	}
	token := strings.TrimPrefix(authHdr, "Bearer ")
	perms, err := a.AuthVerify(token)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid token: %w", err)
	}
	return auth.WithPerm(ctx, perms), perms, nil
}

// methodPermission returns the required permission scope for a JSON-RPC
// method name. Matches the `perm:` tags on FullNodeProxy.Internal.
func methodPermission(method string) auth.Permission {
	// Strip namespace prefix.
	if i := strings.IndexByte(method, '.'); i >= 0 {
		method = method[i+1:]
	}
	switch method {
	case "AuthNew", "Shutdown", "ChainPutObj", "WalletExport", "WalletImport":
		return api.PermAdmin
	case "WalletSign", "WalletSignMessage", "MpoolPushMessage", "MinerCreateBlock", "MarketAddBalance":
		return api.PermSign
	case "WalletNew", "WalletDelete", "WalletSetDefault", "MpoolPush", "SyncSubmitBlock":
		return api.PermWrite
	default:
		return api.PermRead
	}
}

func hasPerm(have []auth.Permission, want auth.Permission) bool {
	for _, p := range have {
		if p == want {
			return true
		}
	}
	return false
}

func jsonRPCError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-32000,"message":%q},"id":null}`, err.Error())
	_, _ = io.WriteString(w, body)
}

// lanternMethodNameFormatter assembles JSON-RPC method names from
// (namespace, GoMethodName) pairs to match the wire-formats real
// clients expect.
//
// - 'Filecoin' namespace: 'Filecoin.MethodName' (Lotus / Curio convention)
// - 'eth' namespace:      'eth_methodName'      (Ethereum / viem / synapse-sdk)
// - everything else:      '<ns>.MethodName'     (default behaviour)
//
// For the 'eth' namespace, the Go method names already follow the
// convention `EthBlockNumber`, `EthChainId`, etc., so this formatter
// strips the 'Eth' prefix and lower-cases the first remaining char:
//   EthBlockNumber → eth_blockNumber
//   EthChainId     → eth_chainId
//   EthGetBalance  → eth_getBalance
//
// Methods that don't start with 'Eth' are still registered (without
// the prefix-strip) so a future RPC method named e.g. `Subscribe`
// would surface as `eth_subscribe`.
func lanternMethodNameFormatter(namespace, method string) string {
	switch namespace {
	case "eth":
		stripped := method
		if len(method) > 3 && method[:3] == "Eth" {
			stripped = method[3:]
		}
		if len(stripped) > 0 {
			// lowercase first char
			stripped = string(stripped[0]|0x20) + stripped[1:]
		}
		return "eth_" + stripped
	case "net":
		stripped := method
		if len(method) > 3 && method[:3] == "Net" {
			stripped = method[3:]
		}
		if len(stripped) > 0 {
			stripped = string(stripped[0]|0x20) + stripped[1:]
		}
		return "net_" + stripped
	}
	return namespace + "." + method
}
