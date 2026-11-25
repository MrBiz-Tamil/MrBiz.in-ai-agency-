package graphql

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/gorilla/websocket"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/mrbiz/abos/backend-core/pkg/auth"
	"github.com/mrbiz/abos/backend-core/pkg/gqlsec"
)

const (
	MaxQueryDepth      = 6
	MaxQueryComplexity = 200
)

type ServerOptions struct {
	Introspection bool                     // development only
	CheckOrigin   func(*http.Request) bool // websocket origin check
	Auth          *auth.Service            // authenticates websocket connections
}

// NewServer builds the GraphQL HTTP + websocket server with the spec's limits
// (depth 6, complexity 200), error scrubbing and authenticated subscriptions.
func NewServer(res *Resolver, opts ServerOptions) *handler.Server {
	srv := handler.New(NewExecutableSchema(Config{Resolvers: res}))

	srv.AddTransport(transport.Websocket{
		KeepAlivePingInterval: 10 * time.Second,
		Upgrader:              websocket.Upgrader{CheckOrigin: opts.CheckOrigin},
		InitFunc: func(ctx context.Context, params transport.InitPayload) (context.Context, *transport.InitPayload, error) {
			if opts.Auth == nil {
				return ctx, nil, auth.ErrUnauthenticated
			}
			// Browsers connect with a short-lived single-use ticket obtained through the BFF.
			if t, ok := params["ticket"].(string); ok && t != "" {
				if p, ok := opts.Auth.RedeemWSTicket(t); ok {
					return auth.WithPrincipal(ctx, p), &params, nil
				}
				return ctx, nil, auth.ErrUnauthenticated
			}
			// Non-browser clients may present a session token directly.
			if tok := strings.TrimPrefix(params.Authorization(), "Bearer "); tok != "" {
				p, err := opts.Auth.Authenticate(ctx, tok)
				if err != nil {
					return ctx, nil, auth.ErrUnauthenticated
				}
				return auth.WithPrincipal(ctx, p), &params, nil
			}
			return ctx, nil, auth.ErrUnauthenticated
		},
	})
	srv.AddTransport(transport.Options{})
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})

	srv.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	if opts.Introspection {
		srv.Use(extension.Introspection{})
	}
	srv.Use(gqlsec.DepthLimit{Max: MaxQueryDepth})
	srv.Use(extension.FixedComplexityLimit(MaxQueryComplexity))
	srv.SetErrorPresenter(PresentError)
	srv.SetRecoverFunc(func(ctx context.Context, err any) error {
		return userErr("INTERNAL", "internal error")
	})
	return srv
}
