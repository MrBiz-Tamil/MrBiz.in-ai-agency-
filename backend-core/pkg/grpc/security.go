package grpc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// maxTokenSkew bounds how old (or future-dated) an internal auth token may be.
const maxTokenSkew = 5 * time.Minute

// MakeAuthToken builds a short-lived HMAC token: "<unix-ts>.<hex(hmac_sha256(secret, ts))>".
func MakeAuthToken(secret string, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	return ts + "." + hex.EncodeToString(mac.Sum(nil))
}

// VerifyAuthToken validates a token produced by MakeAuthToken.
func VerifyAuthToken(secret, token string, now time.Time) error {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return errors.New("malformed token")
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return errors.New("malformed token timestamp")
	}
	issued := time.Unix(sec, 0)
	if d := now.Sub(issued); d > maxTokenSkew || d < -maxTokenSkew {
		return errors.New("token expired")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0]))
	expected := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[1])) != 1 {
		return errors.New("bad signature")
	}
	return nil
}

func authorize(ctx context.Context, secret string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization")
	}
	token := strings.TrimPrefix(vals[0], "Bearer ")
	if err := VerifyAuthToken(secret, token, time.Now()); err != nil {
		return status.Error(codes.Unauthenticated, "invalid authorization")
	}
	return nil
}

// AuthUnaryInterceptor rejects calls without a valid internal HMAC token.
func AuthUnaryInterceptor(secret string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if err := authorize(ctx, secret); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// AuthStreamInterceptor is the streaming counterpart of AuthUnaryInterceptor.
func AuthStreamInterceptor(secret string) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := authorize(ss.Context(), secret); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// RecoveryUnaryInterceptor converts handler panics into Internal errors so one bad
// request can never take the whole kernel down.
func RecoveryUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic in %s: %v\n%s", info.FullMethod, r, debug.Stack())
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// RecoveryStreamInterceptor is the streaming counterpart of RecoveryUnaryInterceptor.
func RecoveryStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic in %s: %v\n%s", info.FullMethod, r, debug.Stack())
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(srv, ss)
	}
}

// ServerOptions returns grpc server options with recovery, optional auth and optional (m)TLS.
// If secret is empty no auth interceptor is installed (development only).
func ServerOptions(secret string, tlsCfg *tls.Config) []grpc.ServerOption {
	unary := []grpc.UnaryServerInterceptor{RecoveryUnaryInterceptor()}
	stream := []grpc.StreamServerInterceptor{RecoveryStreamInterceptor()}
	if secret != "" {
		unary = append(unary, AuthUnaryInterceptor(secret))
		stream = append(stream, AuthStreamInterceptor(secret))
	}
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
	}
	if tlsCfg != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	return opts
}

// LoadTLS builds a TLS config from PEM files. If caFile is set the config requires and
// verifies peer certificates (mutual TLS). Returns (nil, nil) when certFile is empty.
func LoadTLS(certFile, keyFile, caFile string, server bool) (*tls.Config, error) {
	if certFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load key pair: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("no CA certificates found")
		}
		if server {
			cfg.ClientCAs = pool
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
		} else {
			cfg.RootCAs = pool
		}
	}
	return cfg, nil
}

// tokenCreds attaches a fresh HMAC token to every outgoing RPC.
type tokenCreds struct {
	secret     string
	requireTLS bool
}

func (t tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + MakeAuthToken(t.secret, time.Now())}, nil
}

func (t tokenCreds) RequireTransportSecurity() bool { return t.requireTLS }
