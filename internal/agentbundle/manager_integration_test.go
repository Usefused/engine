package agentbundle

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestPortableRuntimeClientToolContinuation verifies skill discovery, client-tool suspension, follow-ups and actor isolation in the real runtime.
func TestPortableRuntimeClientToolContinuation(t *testing.T) {
	archive := os.Getenv("FUSED_TEST_AGENT_ARCHIVE")
	// Ordinary unit tests do not download the optional interpreter.
	if archive == "" {
		t.Skip("portable runtime archive not supplied")
	}
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The runtime must use the configured gateway credential, not its caller's identity.
		if r.Header.Get("Authorization") != "Bearer model-fixture" {
			t.Error("wrong model credential")
			w.WriteHeader(401)
			return
		}
		var input map[string]any
		// Broken request serialization should fail this fixture rather than inventing a response.
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		number := calls.Add(1)
		message := map[string]any{"role": "assistant", "content": "Updated the app name to Checkout."}
		finish := "stop"
		// Tool boundaries must survive suspension and resume before the model sees their results.
		switch number {
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		case 1:
			message = agentToolFixture("skills", "list_skills", `{"query":"workspace drafts"}`)
			finish = "tool_calls"
		// The model loads the exact descriptor returned by the runtime's project skill catalog.
		case 2:
			messages := input["messages"].([]any)
			last := messages[len(messages)-1].(map[string]any)
			var catalog struct {
				Skills []struct {
					ID      string `json:"id"`
					Source  string `json:"source"`
					Version string `json:"version"`
				} `json:"skills"`
			}
			// ADK wraps scalar tool output in a result envelope before sending the provider transcript.
			content := fmt.Sprint(last["content"])
			var envelope struct {
				Result string `json:"result"`
			}
			if json.Unmarshal([]byte(content), &envelope) == nil && envelope.Result != "" {
				content = envelope.Result
			}
			// Invalid skill output is a backend regression, not an excuse to continue without guidance.
			if err := json.Unmarshal([]byte(content), &catalog); err != nil || len(catalog.Skills) == 0 {
				t.Errorf("project skill catalog missing: %v; synthetic result: %s", err, fmt.Sprint(last["content"]))
				w.WriteHeader(500)
				return
			}
			skill := catalog.Skills[0]
			args, _ := json.Marshal(map[string]string{"name": skill.ID, "source": skill.Source, "version": skill.Version})
			message = agentToolFixture("load", "load_skill", string(args))
			finish = "tool_calls"
		// Loading must deliver full skill instructions before the first browser read.
		case 3:
			encoded, _ := json.Marshal(input["messages"])
			if !bytes.Contains(encoded, []byte("Workspace drafts")) {
				t.Error("skill instructions did not reach model")
			}
			message = agentToolFixture("page", "get_page_context", `{}`)
			finish = "tool_calls"
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		case 4:
			message = agentToolFixture("edit", "update_form_field", `{"expected_revision":1,"field_id":"field-1","value":"Checkout"}`)
			finish = "tool_calls"
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		case 6:
			encoded, _ := json.Marshal(input["messages"])
			// The second user message must retain the first turn's conversation in the same session.
			if !bytes.Contains(encoded, []byte("Checkout")) {
				t.Error("follow-up lost session history")
			}
			message["content"] = "The name remains Checkout."
		}
		agentModelFixture(w, input["stream"] == true, message, finish)
	}))
	defer gateway.Close()
	manager, err := New(Options{EngineURL: "http://127.0.0.1", CacheDir: t.TempDir(), ArchivePath: archive, GatewayURL: gateway.URL + "/v1", GatewayKey: "model-fixture", Model: "fused-fixture", IdentityKey: "test-identity-key-with-at-least-32-characters"})
	// Configuration errors should fail before starting a subprocess.
	if err != nil {
		t.Fatal(err)
	}
	manager.Start(context.Background())
	defer manager.Close()
	waitAgentFixture(t, manager)
	endpoint, transport := manager.Endpoint()
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second}
	bearer := "Bearer " + agentTestIdentity("actor-one")
	session := agentRequestFixture(t, client, endpoint+"/sessions", bearer, map[string]any{"state": map[string]string{"title": "Fixture"}})
	first := agentRequestFixture(t, client, endpoint+"/responses", bearer, map[string]any{"input": "Rename the app to Checkout.", "sessionId": session["id"], "stream": true})
	action, _ := first["requiredAction"].(map[string]any)
	// The first frontend action must be a read, before any update can be grounded.
	if action["name"] != "get_page_context" {
		t.Fatalf("missing read action: %#v", first)
	}
	second := agentRequestFixture(t, client, endpoint+"/client-tools/"+fmt.Sprint(action["id"]), bearer, map[string]any{"output": map[string]any{"revision": 1, "fields": []any{map[string]any{"id": "field-1", "label": "Name", "value": "Old", "editable": true}}}})
	action, _ = second["requiredAction"].(map[string]any)
	// Chained frontend tools must remain in the same suspended response.
	if action["name"] != "update_form_field" {
		t.Fatalf("missing edit action: %#v", second)
	}
	result := agentRequestFixture(t, client, endpoint+"/client-tools/"+fmt.Sprint(action["id"]), bearer, map[string]any{"output": map[string]any{"updated": true, "value": "Checkout"}})
	// A tool result must reach the model before final completion.
	if result["status"] != "completed" {
		t.Fatalf("continuation failed: %#v", result)
	}
	followup := agentRequestFixture(t, client, endpoint+"/responses", bearer, map[string]any{"input": "What is the name now?", "sessionId": session["id"], "stream": true})
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if followup["status"] != "completed" || calls.Load() != 6 {
		t.Fatalf("follow-up failed: %#v calls=%d", followup, calls.Load())
	}
	request, _ := http.NewRequest("GET", endpoint+"/sessions/"+fmt.Sprint(session["id"]), nil)
	request.Header.Set("Authorization", "Bearer "+agentTestIdentity("actor-two"))
	response, err := client.Do(request)
	// Knowing a session ID never grants another actor access to its page context.
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if response.StatusCode != 403 && response.StatusCode != 404 {
		t.Fatalf("cross-actor session access: %d", response.StatusCode)
	}
}

// agentToolFixture builds a provider response in the same tool-call shape used by Registry streaming.
func agentToolFixture(id, name, args string) map[string]any {
	return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}
}

// agentModelFixture supports both initial streaming and Harnest's JSON client-tool continuation.
func agentModelFixture(w http.ResponseWriter, stream bool, message map[string]any, finish string) {
	response := map[string]any{"id": "chat-fixture", "object": "chat.completion", "created": 1234567, "model": "fused-fixture", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}}
	// Streaming uses complete bounded chunks so the fixture is deterministic across runtimes.
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		response["object"] = "chat.completion.chunk"
		response["choices"] = []any{map[string]any{"index": 0, "delta": message, "finish_reason": nil}}
		data, _ := json.Marshal(response)
		fmt.Fprintf(w, "data: %s\n\n", data)
		response["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}
		data, _ = json.Marshal(response)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// waitAgentFixture bounds native startup and keeps diagnostics available only to the test runner.
func waitAgentFixture(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		status := m.Status()
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if status.State == "ready" {
			return
		} // Readiness requires a responding pinned-TLS child.
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if status.State == "unavailable" {
			data, _ := os.ReadFile(m.DiagnosticsPath())
			t.Fatalf("%s: %s", status.Message, data)
		} // Surface startup errors once.
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("agent startup timed out")
}

// agentRequestFixture returns the completed or suspended response from either supported transport.
func agentRequestFixture(t *testing.T, client *http.Client, url, bearer string, payload any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(payload)
	request, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	request.Header.Set("Authorization", bearer)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err != nil {
		t.Fatal(err)
	} // Network failures cannot become empty successful fixtures.
	defer response.Body.Close()
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if response.StatusCode >= 300 {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("%s: %d %s", url, response.StatusCode, data)
	} // Reject protocol errors before decoding.
	var result map[string]any
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if !strings.Contains(response.Header.Get("Content-Type"), "event-stream") { // Continuations return JSON.
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if !strings.HasPrefix(line, "data:") {
			continue
		} // Ignore SSE framing rather than parsing it as JSON.
		var event map[string]any
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			t.Fatal(err)
		}
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if event["type"] == "response.completed" {
			result = event
		} // Preserve only the authoritative terminal event.
		// Fail the fixture when this invariant would weaken runtime isolation or verification.
		if event["type"] == "error" {
			t.Fatalf("agent response error: %#v", event)
		}
	}
	// Fail the fixture when this invariant would weaken runtime isolation or verification.
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	} // Truncated streams are not successful replies.
	return result
}

// agentTestIdentity reproduces the Engine's signed actor boundary without needing workspace credentials.
func agentTestIdentity(subject string) string {
	body, _ := json.Marshal(map[string]any{"sub": subject, "workspace_id": "test-workspace", "iss": "fused-engine", "aud": "fused-agent", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
	raw := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte("test-identity-key-with-at-least-32-characters"))
	mac.Write([]byte(raw))
	return raw + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
