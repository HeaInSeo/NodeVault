package grpcauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/HeaInSeo/NodeVault/pkg/grpcauth"
)

const handlerOKResponse = "ok"

func ctxWithToken(token string) context.Context {
	ctx := context.Background()
	if token == "" {
		return ctx
	}
	return metadata.NewIncomingContext(ctx, metadata.Pairs(grpcauth.TokenMetadataKey, token))
}

func TestFromEnv_UnsetDisablesAuth(t *testing.T) {
	t.Setenv(grpcauth.SharedSecretEnv, "")
	secret, ok := grpcauth.FromEnv()
	if ok {
		t.Errorf("expected ok=false for unset env, got secret=%q ok=%v", secret, ok)
	}
}

func TestFromEnv_SetEnablesAuth(t *testing.T) {
	t.Setenv(grpcauth.SharedSecretEnv, "s3cr3t")
	secret, ok := grpcauth.FromEnv()
	if !ok || secret != "s3cr3t" {
		t.Errorf("got secret=%q ok=%v, want secret=s3cr3t ok=true", secret, ok)
	}
}

func TestUnaryInterceptor_ValidToken_CallsHandler(t *testing.T) {
	interceptor := grpcauth.UnaryInterceptor("s3cr3t")
	called := false
	handler := func(_ context.Context, _ any) (any, error) {
		called = true
		return handlerOKResponse, nil
	}
	resp, err := interceptor(ctxWithToken("s3cr3t"), nil, &grpc.UnaryServerInfo{}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("handler was not called")
	}
	if resp != handlerOKResponse {
		t.Errorf("resp = %v, want %s", resp, handlerOKResponse)
	}
}

func TestUnaryInterceptor_MissingToken_Rejected(t *testing.T) {
	interceptor := grpcauth.UnaryInterceptor("s3cr3t")
	called := false
	handler := func(_ context.Context, _ any) (any, error) {
		called = true
		return handlerOKResponse, nil
	}
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("status = %v, want Unauthenticated", status.Code(err))
	}
	if called {
		t.Error("handler must not be called when token is missing")
	}
}

func TestUnaryInterceptor_WrongToken_Rejected(t *testing.T) {
	interceptor := grpcauth.UnaryInterceptor("s3cr3t")
	called := false
	handler := func(_ context.Context, _ any) (any, error) {
		called = true
		return handlerOKResponse, nil
	}
	_, err := interceptor(ctxWithToken("wrong"), nil, &grpc.UnaryServerInfo{}, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("status = %v, want Unauthenticated", status.Code(err))
	}
	if called {
		t.Error("handler must not be called when token is wrong")
	}
}

func TestUnaryInterceptor_EmptyToken_Rejected(t *testing.T) {
	interceptor := grpcauth.UnaryInterceptor("s3cr3t")
	handler := func(_ context.Context, _ any) (any, error) { return handlerOKResponse, nil }
	_, err := interceptor(ctxWithToken(""), nil, &grpc.UnaryServerInfo{}, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("status = %v, want Unauthenticated", status.Code(err))
	}
}

// fakeServerStream is a minimal grpc.ServerStream carrying a fixed context,
// enough to exercise StreamInterceptor without a real network connection.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }

func TestStreamInterceptor_ValidToken_CallsHandler(t *testing.T) {
	interceptor := grpcauth.StreamInterceptor("s3cr3t")
	called := false
	handler := func(_ any, _ grpc.ServerStream) error {
		called = true
		return nil
	}
	err := interceptor(nil, &fakeServerStream{ctx: ctxWithToken("s3cr3t")}, &grpc.StreamServerInfo{}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("handler was not called")
	}
}

func TestStreamInterceptor_MissingToken_Rejected(t *testing.T) {
	interceptor := grpcauth.StreamInterceptor("s3cr3t")
	called := false
	handler := func(_ any, _ grpc.ServerStream) error {
		called = true
		return nil
	}
	err := interceptor(nil, &fakeServerStream{ctx: context.Background()}, &grpc.StreamServerInfo{}, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("status = %v, want Unauthenticated", status.Code(err))
	}
	if called {
		t.Error("handler must not be called when token is missing")
	}
}

func TestHTTPMiddleware_ValidToken_CallsNext(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := grpcauth.HTTPMiddleware("s3cr3t", next)

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/harbor", http.NoBody)
	req.Header.Set(grpcauth.HTTPHeaderName, "s3cr3t")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler was not called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHTTPMiddleware_MissingToken_Rejected(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := grpcauth.HTTPMiddleware("s3cr3t", next)

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/harbor", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("next handler must not be called when token is missing")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHTTPMiddleware_WrongToken_Rejected(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := grpcauth.HTTPMiddleware("s3cr3t", next)

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/harbor", http.NoBody)
	req.Header.Set(grpcauth.HTTPHeaderName, "wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("next handler must not be called when token is wrong")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
