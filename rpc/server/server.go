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

	rs := jsonrpc.NewServer()

	// Register the FullNode directly. Lotus uses a permission-decorated
	// proxy struct (FullNodeStruct) with auto-generated method shims; for
	// Lantern V1 we enforce perms at the HTTP middleware (Auth.handle) by
	// mapping method name prefixes to required scope.
	rs.Register("Filecoin", node)

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
			var req struct{ Method string `json:"method"` }
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
type authPayload struct {
	Allow []auth.Permission
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
func (a *Auth) mint(perms []auth.Permission) (string, error) {
	hs := gjwt.NewHS256(a.secret)
	payload := authPayload{Allow: perms}
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
func (a *Auth) AuthVerify(token string) ([]auth.Permission, error) {
	var p authPayload
	if _, err := gjwt.Verify([]byte(token), gjwt.NewHS256(a.secret), &p); err != nil {
		return nil, err
	}
	return p.Allow, nil
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
