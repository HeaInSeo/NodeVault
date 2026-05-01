// smoke_client.go — NodeVault gRPC + NodePalette REST 연동 검증
//
// 실행:
//   go run smoke_client.go
//   NODEVAULT_ADDR=100.123.80.48:50051 NODEPALETTE_ADDR=100.123.80.48:8080 go run smoke_client.go
//
// 사전 조건: NodeVault(:50051)과 NodePalette(:8080)가 실행 중이어야 한다.

//go:build ignore

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	nfv1 "github.com/HeaInSeo/NodeVault/protos/nodevault/v1"
)

func grpcAddr() string {
	if v := os.Getenv("NODEVAULT_ADDR"); v != "" {
		return v
	}
	return "localhost:50051"
}

func paletteAddr() string {
	if v := os.Getenv("NODEPALETTE_ADDR"); v != "" {
		return v
	}
	return "localhost:8080"
}

func main() {
	conn, err := grpc.NewClient(grpcAddr(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pass := true

	// ── 1. Ping ───────────────────────────────────────────────────────────────
	pingClient := nfv1.NewPingServiceClient(conn)
	pingResp, err := pingClient.Ping(ctx, &nfv1.PingRequest{Message: "hello"})
	if err != nil {
		fmt.Printf("[FAIL] Ping: %v\n", err)
		pass = false
	} else {
		fmt.Printf("[PASS] Ping → message=%q serverId=%q\n", pingResp.Message, pingResp.ServerId)
	}

	// ── 2. ListPolicies ───────────────────────────────────────────────────────
	policyClient := nfv1.NewPolicyServiceClient(conn)
	listResp, err := policyClient.ListPolicies(ctx, &nfv1.ListPoliciesRequest{})
	if err != nil {
		fmt.Printf("[FAIL] ListPolicies: %v\n", err)
		pass = false
	} else {
		fmt.Printf("[PASS] ListPolicies → bundleVersion=%q policies=%d\n",
			listResp.BundleVersion, len(listResp.Policies))
		for _, p := range listResp.Policies {
			fmt.Printf("       rule=%s name=%q\n", p.RuleId, p.Name)
		}
	}

	// ── 3. GetPolicyBundle ────────────────────────────────────────────────────
	bundleResp, err := policyClient.GetPolicyBundle(ctx, &nfv1.GetPolicyBundleRequest{})
	if err != nil {
		fmt.Printf("[FAIL] GetPolicyBundle: %v\n", err)
		pass = false
	} else {
		fmt.Printf("[PASS] GetPolicyBundle → version=%q wasmBytes=%d builtAt=%d\n",
			bundleResp.Version, len(bundleResp.WasmBytes), bundleResp.BuiltAt)
		if len(bundleResp.WasmBytes) == 0 {
			fmt.Println("[WARN] wasmBytes is empty!")
			pass = false
		}
	}

	// ── 4. ListTools gRPC (CAS 직접 조회) ──────────────────────────────────────
	registryClient := nfv1.NewToolRegistryServiceClient(conn)
	toolsResp, err := registryClient.ListTools(ctx, &nfv1.ListToolsRequest{})
	if err != nil {
		fmt.Printf("[FAIL] ListTools(gRPC): %v\n", err)
		pass = false
	} else {
		fmt.Printf("[PASS] ListTools(gRPC) → count=%d\n", len(toolsResp.Tools))
		for _, t := range toolsResp.Tools {
			fmt.Printf("       cas=%s stableRef=%q phase=%s health=%s\n",
				t.CasHash[:min(12, len(t.CasHash))], t.StableRef, t.LifecyclePhase, t.IntegrityHealth)
		}
	}

	// ── 5. NodePalette REST — /v1/catalog/tools ──────────────────────────────
	paletteURL := "http://" + paletteAddr() + "/v1/catalog/tools"
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, paletteURL, http.NoBody)
	if reqErr != nil {
		fmt.Printf("[FAIL] NodePalette request build: %v\n", reqErr)
		pass = false
	} else {
		resp, httpErr := http.DefaultClient.Do(req)
		if httpErr != nil {
			fmt.Printf("[FAIL] NodePalette GET %s: %v\n", paletteURL, httpErr)
			pass = false
		} else {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				fmt.Printf("[FAIL] NodePalette → HTTP %d: %s\n", resp.StatusCode, string(body))
				pass = false
			} else {
				var result struct {
					Tools []json.RawMessage `json:"tools"`
				}
				if jsonErr := json.Unmarshal(body, &result); jsonErr != nil {
					fmt.Printf("[FAIL] NodePalette response parse: %v\n", jsonErr)
					pass = false
				} else {
					fmt.Printf("[PASS] NodePalette GET /v1/catalog/tools → count=%d\n", len(result.Tools))
				}
			}
		}
	}

	// ── 6. NodePalette REST — /v1/catalog/data ───────────────────────────────
	dataURL := "http://" + paletteAddr() + "/v1/catalog/data"
	dataReq, dataReqErr := http.NewRequestWithContext(ctx, http.MethodGet, dataURL, http.NoBody)
	if dataReqErr != nil {
		fmt.Printf("[FAIL] NodePalette data request build: %v\n", dataReqErr)
		pass = false
	} else {
		dataResp, dataErr := http.DefaultClient.Do(dataReq)
		if dataErr != nil {
			fmt.Printf("[FAIL] NodePalette GET %s: %v\n", dataURL, dataErr)
			pass = false
		} else {
			defer dataResp.Body.Close()
			if dataResp.StatusCode != http.StatusOK {
				fmt.Printf("[FAIL] NodePalette /v1/catalog/data → HTTP %d\n", dataResp.StatusCode)
				pass = false
			} else {
				fmt.Printf("[PASS] NodePalette GET /v1/catalog/data → HTTP 200\n")
			}
		}
	}

	// ── 결과 ──────────────────────────────────────────────────────────────────
	fmt.Println()
	if pass {
		fmt.Println("=== SMOKE: ALL PASS ===")
	} else {
		fmt.Println("=== SMOKE: FAILED ===")
		log.Fatal("smoke test failed")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
