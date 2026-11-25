package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	"github.com/mrbiz/abos/backend-core/pkg/vault"
)

// Store is the persistence the gateway needs (satisfied by *repository.Repository).
type Store interface {
	FindMCPConnectionByName(ctx context.Context, workspaceID, serverName string) (*repository.MCPConnection, error)
	GetMCPConnection(ctx context.Context, workspaceID, id string) (*repository.MCPConnection, error)
	SetMCPHealth(ctx context.Context, workspaceID, id, health string) error
}

// Gateway routes semantic tool names ("shopify.get_order") to the workspace's MCP server
// named "shopify", checks the connection's allowed scopes, injects decrypted credentials
// and calls the server. Satisfies tickets.Executor and grpc.ToolExecutor.
type Gateway struct {
	store  Store
	vault  *vault.Vault
	client *Client
}

func NewGateway(store Store, v *vault.Vault, client *Client) *Gateway {
	return &Gateway{store: store, vault: v, client: client}
}

var (
	ErrNoConnection  = errors.New("no MCP connection is registered for this tool")
	ErrScopeDenied   = errors.New("tool is not in the connection's allowed scopes")
	ErrInvalidToolID = errors.New("tool name must look like <server>.<tool>")
)

// SplitTool splits "shopify.get_order" into ("shopify", "get_order").
func SplitTool(name string) (server, tool string, err error) {
	i := strings.Index(name, ".")
	if i <= 0 || i == len(name)-1 {
		return "", "", ErrInvalidToolID
	}
	return name[:i], name[i+1:], nil
}

// scopeAllows reports whether scopes contain the full tool name, "<server>.*" or "*".
func scopeAllows(scopes []string, server, fullName string) bool {
	for _, s := range scopes {
		if s == "*" || s == fullName || s == server+".*" {
			return true
		}
	}
	return false
}

func (g *Gateway) serverFor(ctx context.Context, conn *repository.MCPConnection) (Server, error) {
	plain, err := g.vault.Decrypt(conn.WorkspaceID, conn.EncryptedAuthRef)
	if err != nil {
		return Server{}, fmt.Errorf("credentials for %q cannot be decrypted", conn.ServerName)
	}
	var creds Credentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return Server{}, errors.New("stored credentials are malformed")
	}
	return Server{URI: conn.ServerURI, Creds: creds}, nil
}

func (g *Gateway) Execute(ctx context.Context, workspaceID, toolName, payloadJSON string) (string, error) {
	serverName, tool, err := SplitTool(toolName)
	if err != nil {
		return "", err
	}
	conn, err := g.store.FindMCPConnectionByName(ctx, workspaceID, serverName)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return "", fmt.Errorf("%w: %q", ErrNoConnection, serverName)
		}
		return "", err
	}
	var scopes []string
	_ = json.Unmarshal(conn.AllowedScopes, &scopes)
	if !scopeAllows(scopes, serverName, toolName) {
		return "", fmt.Errorf("%w: %s", ErrScopeDenied, toolName)
	}
	srv, err := g.serverFor(ctx, conn)
	if err != nil {
		return "", err
	}
	return g.client.CallTool(ctx, srv, tool, json.RawMessage(payloadJSON))
}

// CheckHealth pings a connection and records the result.
// Reachable and speaking MCP => HEALTHY; reachable but erroring/unauthorised => DEGRADED;
// unreachable => CRITICAL.
func (g *Gateway) CheckHealth(ctx context.Context, workspaceID, connectionID string) (string, error) {
	conn, err := g.store.GetMCPConnection(ctx, workspaceID, connectionID)
	if err != nil {
		return "", err
	}
	health := "HEALTHY"
	srv, err := g.serverFor(ctx, conn)
	if err != nil {
		health = "CRITICAL"
	} else if perr := g.client.Ping(ctx, srv); perr != nil {
		var hs ErrHTTPStatus
		if errors.As(perr, &hs) || strings.HasPrefix(perr.Error(), "MCP error") {
			health = "DEGRADED"
		} else {
			health = "CRITICAL"
		}
	}
	if err := g.store.SetMCPHealth(ctx, workspaceID, connectionID, health); err != nil {
		return "", err
	}
	return health, nil
}
