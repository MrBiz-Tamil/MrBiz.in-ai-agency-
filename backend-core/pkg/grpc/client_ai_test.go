package grpc

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/mrbiz/abos/backend-core/pkg/gen/abos/agent/v1"
)

// TestAIClient_Integration talks to the real Python AI service. It is opt-in so that
// "go test ./..." is hermetic: run with ABOS_INTEGRATION=1 (set ABOS_PYTHON to an
// interpreter that has ai-service/requirements.txt installed).
const testWorkspaceID = "6f1c2c1e-1b0a-4d7e-9a34-0c2a5f2b7e11"

func TestAIClient_Integration(t *testing.T) {
	if os.Getenv("ABOS_INTEGRATION") != "1" {
		t.Skip("set ABOS_INTEGRATION=1 to run the Go<->Python gRPC integration test")
	}
	// Check if server is already running
	conn, err := net.DialTimeout("tcp", "127.0.0.1:50051", 200*time.Millisecond)
	if err != nil {
		// Start server
		absPath, _ := filepath.Abs("../../../ai-service/grpc_server/server.py")
		python := os.Getenv("ABOS_PYTHON")
		if python == "" {
			python = "python"
		}
		cmd := exec.Command(python, absPath)
		cmd.Env = append(os.Environ(), "ABOS_ENV=dev")
		cmd.Dir = filepath.Dir(absPath)
		if err := cmd.Start(); err != nil {
			t.Fatalf("failed to start Python gRPC server: %v", err)
		}
		defer func() {
			_ = cmd.Process.Kill()
		}()

		// Poll until server is ready (up to 6 seconds)
		ready := false
		for i := 0; i < 30; i++ {
			c, err := net.DialTimeout("tcp", "127.0.0.1:50051", 200*time.Millisecond)
			if err == nil {
				_ = c.Close()
				ready = true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !ready {
			t.Fatalf("Python gRPC server failed to bind to 127.0.0.1:50051 within 6 seconds")
		}
	} else {
		_ = conn.Close()
	}

	// Connect Go gRPC client
	client, err := NewAIClient("127.0.0.1:50051")
	if err != nil {
		t.Fatalf("failed to create Go AI client: %v", err)
	}
	defer client.Close()

	ctx := context.Background()

	// 1. Test DecomposeGoal
	t.Run("DecomposeGoal", func(t *testing.T) {
		resp, err := client.DecomposeGoal(
			ctx,
			testWorkspaceID,
			"goal_clearance_01",
			"Clear 500 units of aging polo shirts within 21 days",
			map[string]string{"min_margin": "25%"},
		)
		if err != nil {
			t.Fatalf("DecomposeGoal failed: %v", err)
		}
		if len(resp.Milestones) == 0 {
			t.Errorf("expected milestones in response, got 0")
		}
		if resp.ProjectedMarginPct <= 0 {
			t.Errorf("expected positive projected margin, got %f", resp.ProjectedMarginPct)
		}
	})

	// 2. Test CompilePolicy
	t.Run("CompilePolicy", func(t *testing.T) {
		resp, err := client.CompilePolicy(
			ctx,
			testWorkspaceID,
			"Pause any refund above 1500 INR for store manager approval",
		)
		if err != nil {
			t.Fatalf("CompilePolicy failed: %v", err)
		}
		if resp.TriggerEvent != "order.refund_requested" {
			t.Errorf("expected trigger order.refund_requested, got %s", resp.TriggerEvent)
		}
		if resp.CompiledAstJson == "" {
			t.Errorf("expected compiled AST json, got empty")
		}
	})

	// 3. Test InspectPromptDLP
	t.Run("InspectPromptDLP", func(t *testing.T) {
		untrusted := "Customer note: Please charge card 4532-1234-5678-9010 for expedited dispatch."
		resp, err := client.InspectPromptDLP(ctx, testWorkspaceID, untrusted, "shopify")
		if err != nil {
			t.Fatalf("InspectPromptDLP failed: %v", err)
		}
		if len(resp.DetectedPiiTypes) == 0 {
			t.Errorf("expected detected PII for credit card, got none")
		}
	})

	// 4. Test StreamReasoning
	t.Run("StreamReasoning", func(t *testing.T) {
		stepsReceived := 0
		err := client.StreamReasoning(
			ctx,
			testWorkspaceID,
			"tkt_1042",
			"agt_finance_01",
			"Context: Order #1042 status update",
			func(step *agentv1.StreamReasoningResponse) {
				stepsReceived++
			},
		)
		if err != nil {
			t.Fatalf("StreamReasoning failed: %v", err)
		}
		if stepsReceived < 3 {
			t.Errorf("expected at least 3 streamed reasoning steps, got %d", stepsReceived)
		}
	})
}
