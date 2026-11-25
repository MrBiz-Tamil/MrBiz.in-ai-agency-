package grpc

import (
	"context"
	"crypto/tls"
	"io"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/mrbiz/abos/backend-core/pkg/gen/abos/agent/v1"
)

type AIClient struct {
	conn   *grpc.ClientConn
	client agentv1.AgentOrchestratorServiceClient
}

// ClientConfig configures the connection to the AI engine.
type ClientConfig struct {
	Addr   string      // defaults to 127.0.0.1:50051
	Secret string      // internal HMAC secret; if empty no auth token is sent (development only)
	TLS    *tls.Config // nil => plaintext (only acceptable on loopback / trusted overlay)
}

// ClientConfigFromEnv reads ABOS_AI_ADDR, ABOS_INTERNAL_SECRET and ABOS_TLS_{CERT,KEY,CA}.
func ClientConfigFromEnv() (ClientConfig, error) {
	tlsCfg, err := LoadTLS(os.Getenv("ABOS_TLS_CERT"), os.Getenv("ABOS_TLS_KEY"), os.Getenv("ABOS_TLS_CA"), false)
	if err != nil {
		return ClientConfig{}, err
	}
	return ClientConfig{
		Addr:   os.Getenv("ABOS_AI_ADDR"),
		Secret: os.Getenv("ABOS_INTERNAL_SECRET"),
		TLS:    tlsCfg,
	}, nil
}

// NewAIClient keeps the simple constructor for local use / tests: plaintext, secret from env.
func NewAIClient(targetAddr string) (*AIClient, error) {
	return NewAIClientWithConfig(ClientConfig{Addr: targetAddr, Secret: os.Getenv("ABOS_INTERNAL_SECRET")})
}

func NewAIClientWithConfig(cfg ClientConfig) (*AIClient, error) {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:50051"
	}

	opts := []grpc.DialOption{}
	if cfg.TLS != nil {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(cfg.TLS)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	if cfg.Secret != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(tokenCreds{secret: cfg.Secret, requireTLS: false}))
	}

	conn, err := grpc.NewClient(cfg.Addr, opts...)
	if err != nil {
		return nil, err
	}
	return &AIClient{conn: conn, client: agentv1.NewAgentOrchestratorServiceClient(conn)}, nil
}

func (c *AIClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// DecomposeGoal dispatches a high-level goal to FastAPI AI engine.
func (c *AIClient) DecomposeGoal(
	ctx context.Context,
	workspaceID string,
	goalID string,
	description string,
	constraints map[string]string,
) (*agentv1.DecomposeGoalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return c.client.DecomposeGoal(ctx, &agentv1.DecomposeGoalRequest{
		WorkspaceId:     workspaceID,
		GoalId:          goalID,
		GoalDescription: description,
		Constraints:     constraints,
	})
}

// CompilePolicy compiles plain English into a deterministic AST.
func (c *AIClient) CompilePolicy(
	ctx context.Context,
	workspaceID string,
	rawPolicyText string,
) (*agentv1.CompilePolicyResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return c.client.CompilePolicy(ctx, &agentv1.CompilePolicyRequest{
		WorkspaceId:   workspaceID,
		RawPolicyText: rawPolicyText,
	})
}

// InspectPromptDLP screens untrusted strings for PII and injection.
func (c *AIClient) InspectPromptDLP(
	ctx context.Context,
	workspaceID string,
	content string,
	originSystem string,
) (*agentv1.InspectPromptDLPResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	return c.client.InspectPromptDLP(ctx, &agentv1.InspectPromptDLPRequest{
		WorkspaceId:      workspaceID,
		UntrustedContent: content,
		OriginSystem:     originSystem,
	})
}

// StreamReasoning steps from Python agent back to Go.
func (c *AIClient) StreamReasoning(
	ctx context.Context,
	workspaceID string,
	ticketID string,
	agentID string,
	promptContext string,
	onStep func(*agentv1.StreamReasoningResponse),
) error {
	stream, err := c.client.StreamReasoning(ctx, &agentv1.StreamReasoningRequest{
		WorkspaceId:   workspaceID,
		TicketId:      ticketID,
		AgentId:       agentID,
		PromptContext: promptContext,
	})
	if err != nil {
		return err
	}

	for {
		step, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		onStep(step)
	}
	return nil
}

// RunAgentHeartbeat asks the AI engine to run one proactive check for an agent.
func (c *AIClient) RunAgentHeartbeat(
	ctx context.Context,
	workspaceID string,
	agentID string,
	agentRole string,
) (*agentv1.RunAgentHeartbeatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return c.client.RunAgentHeartbeat(ctx, &agentv1.RunAgentHeartbeatRequest{
		WorkspaceId: workspaceID,
		AgentId:     agentID,
		AgentRole:   agentRole,
	})
}
