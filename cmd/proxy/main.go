// Command proxy is a drop-in replacement for `bazel run
// //services/db/server:server_public` that forwards every request to a real
// Twisp tenant in the cloud. The local listener is unauthenticated; the proxy
// attaches Authorization / X-Twisp-Account-Id headers on the way out using
// AWS IAM Outbound Identity Federation.
//
// In its default mode it points at an existing tenant (-account-id or
// -tenant-file). With -vend-account it instead spins up a fresh ephemeral
// tenant on a parent "vend" tenant at startup, runs the proxy against it,
// and deletes it on SIGTERM/SIGINT.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/siderolabs/grpc-proxy/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/parsnips/ephemeral/auth"
	"github.com/parsnips/ephemeral/vend"
)

func main() {
	var (
		region    = flag.String("region", envOr("AWS_REGION", "us-east-1"), "AWS / Twisp region")
		twispEnv  = flag.String("env", envOr("TWISP_ENV", "cloud"), "Twisp environment (cloud, dev, ...)")
		audience  = flag.String("audience", envOr("AUDIENCE", "ephemeral"), "JWT audience claim")
		httpAddr  = flag.String("http", ":8080", "address to listen on for HTTP/GraphQL")
		grpcAddr  = flag.String("grpc", ":8081", "address to listen on for gRPC")
		vendAcct  = flag.String("vend-account", os.Getenv("VEND_ACCOUNT_ID"), "if set, vend a fresh ephemeral tenant on this parent at startup and reap on shutdown")
		vendPfx   = flag.String("prefix", envOr("EPHEMERAL_PREFIX", "ephemeral"), "name prefix for the vended ephemeral tenant")
		accountID = flag.String("account-id", os.Getenv("EPHEMERAL_ACCOUNT_ID"), "tenant accountId to proxy to (mutually exclusive with -vend-account / -tenant-file)")
		tenantF   = flag.String("tenant-file", os.Getenv("TENANT_FILE"), "read EPHEMERAL_ACCOUNT_ID from this file (used when running side-by-side with cmd/vend)")
		fileWait  = flag.Duration("wait", 30*time.Second, "how long to wait for -tenant-file to appear")
	)
	flag.Parse()

	switch {
	case *vendAcct != "" && (*accountID != "" || *tenantF != ""):
		log.Fatal("-vend-account is mutually exclusive with -account-id and -tenant-file")
	case *vendAcct == "" && *accountID == "" && *tenantF == "":
		log.Fatal("set one of -vend-account, -account-id, or -tenant-file")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region))
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}
	source := auth.NewTokenSource(sts.NewFromConfig(cfg), *audience)

	target, cleanup, err := resolveTenant(ctx, *vendAcct, *accountID, *tenantF, *fileWait, *region, *twispEnv, *vendPfx, source)
	if err != nil {
		log.Fatalf("resolve tenant: %v", err)
	}
	if cleanup != nil {
		// Always run cleanup, even if the servers fail to start.
		defer cleanup()
	}
	log.Printf("proxy targeting tenant accountId=%s region=%s env=%s", target, *region, *twispEnv)

	httpHost := fmt.Sprintf("api.%s.%s.twisp.com", *region, *twispEnv)
	grpcHost := fmt.Sprintf("api.%s.%s.twisp.com:50051", *region, *twispEnv)

	httpSrv := buildHTTPServer(*httpAddr, httpHost, source, target)
	grpcSrv, grpcLis, err := buildGRPCServer(*grpcAddr, grpcHost, source, target)
	if err != nil {
		log.Fatalf("grpc server: %v", err)
	}

	go func() {
		log.Printf("HTTP listening on %s -> https://%s", *httpAddr, httpHost)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()
	go func() {
		log.Printf("gRPC listening on %s -> %s", *grpcAddr, grpcHost)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			log.Fatalf("grpc: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutdown, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	_ = httpSrv.Shutdown(shutdown)
	grpcSrv.GracefulStop()
}

// resolveTenant picks the accountId to proxy to. If vendAcct is non-empty it
// creates a fresh tenant via the vend library and returns a cleanup func that
// deletes it. Otherwise it returns the static accountID or reads the file.
func resolveTenant(ctx context.Context, vendAcct, accountID, file string, wait time.Duration, region, env, prefix string, source *auth.TokenSource) (string, func(), error) {
	if vendAcct != "" {
		v, err := vend.New(vend.Config{
			Region:        region,
			Env:           env,
			VendAccountID: vendAcct,
			Prefix:        prefix,
			Source:        source,
		})
		if err != nil {
			return "", nil, err
		}
		log.Printf("vending ephemeral tenant on parent=%s", vendAcct)
		t, err := v.Create(ctx)
		if err != nil {
			return "", nil, err
		}
		log.Printf("vended ephemeral tenant accountId=%s id=%s", t.AccountID, t.ID)

		cleanup := func() {
			td, c := context.WithTimeout(context.Background(), 60*time.Second)
			defer c()
			log.Printf("deleting ephemeral tenant accountId=%s", t.AccountID)
			if err := v.Delete(td, t.AccountID); err != nil {
				log.Printf("deleteTenant: %v", err)
				return
			}
			log.Printf("deleted ephemeral tenant accountId=%s", t.AccountID)
		}
		return t.AccountID, cleanup, nil
	}

	if accountID != "" {
		return accountID, nil, nil
	}

	id, err := waitForAccountID(ctx, file, wait)
	return id, nil, err
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// waitForAccountID polls path until it contains EPHEMERAL_ACCOUNT_ID=…
// or the timeout elapses. Tolerant of starting before the file is written.
func waitForAccountID(ctx context.Context, path string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(path); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "EPHEMERAL_ACCOUNT_ID="); ok && v != "" {
					return v, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for %s", path)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func buildHTTPServer(addr, upstreamHost string, source *auth.TokenSource, accountID string) *http.Server {
	target := &url.URL{Scheme: "https", Host: upstreamHost}
	rp := httputil.NewSingleHostReverseProxy(target)

	director := rp.Director
	rp.Director = func(req *http.Request) {
		director(req)
		req.Host = upstreamHost
		// Strip any inbound auth — the upstream auth comes from our RoundTripper.
		req.Header.Del(auth.HeaderAuthorization)
		req.Header.Del(auth.HeaderAccountID)
	}
	rp.Transport = auth.NewRoundTripper(source, accountID, http.DefaultTransport)

	return &http.Server{
		Addr:    addr,
		Handler: rp,
	}
}

func buildGRPCServer(addr, upstreamHost string, source *auth.TokenSource, accountID string) (*grpc.Server, net.Listener, error) {
	creds := credentials.NewTLS(&tls.Config{ServerName: stripPort(upstreamHost)})
	perRPC := &auth.GRPCPerRPC{Source: source, AccountID: accountID}

	conn, err := grpc.NewClient(upstreamHost,
		grpc.WithDefaultCallOptions(grpc.ForceCodecV2(proxy.Codec())),
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(perRPC),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial upstream grpc: %w", err)
	}

	backend := &proxy.SingleBackend{
		GetConn: func(ctx context.Context) (context.Context, *grpc.ClientConn, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			md = md.Copy()
			md.Delete("authorization")
			md.Delete("x-twisp-account-id")
			return metadata.NewOutgoingContext(ctx, md), conn, nil
		},
	}

	director := func(ctx context.Context, fullMethodName string) (proxy.Mode, []proxy.Backend, error) {
		return proxy.One2One, []proxy.Backend{backend}, nil
	}

	server := grpc.NewServer(
		grpc.ForceServerCodecV2(proxy.Codec()),
		grpc.UnknownServiceHandler(proxy.TransparentHandler(director)),
	)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	return server, lis, nil
}

func stripPort(hostport string) string {
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		return hostport[:i]
	}
	return hostport
}
