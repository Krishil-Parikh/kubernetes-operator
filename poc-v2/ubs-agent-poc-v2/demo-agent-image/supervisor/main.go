// agent-supervisor is the container's PID 1. It owns the agent's entire
// register -> serve -> deregister lifecycle itself, instead of delegating
// pieces of it to Kubernetes postStart/preStop exec hooks:
//
//   - Registration happens before nginx is even started, so there is no
//     window where an unregistered pod can receive traffic (previously
//     enforced by postStart + readinessProbe; now enforced by plain
//     program order in this one process).
//   - Deregistration happens on our own SIGTERM handler before nginx is
//     asked to stop (previously the preStop hook's job).
//   - BLUEPRINT_ID / AGENT_IMAGE / UBS_API_BASE come from a .env file
//     baked into the image at build time (see loadDotEnv), not from a
//     ConfigMap or inline Deployment env: values -- only the credential
//     (BLUEPRINT_TOKEN) still comes from a real Kubernetes Secret, since
//     baking a token into an image layer would be a genuine security
//     problem, not a shortcut worth avoiding.
//
// Because this program is the container's actual main process, everything
// it prints goes to the container's real stdout -- the same stream nginx's
// own access/error logs go to -- so `kubectl logs` shows the whole
// lifecycle narrative. Hook-exec output never had that guarantee.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	registerMaxAttempts = 10
	registerRetryDelay  = 3 * time.Second
	deregisterAttempts  = 5
	deregisterRetryWait = 2 * time.Second
)

var healthy atomic.Bool

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "%s agent-supervisor: %s\n",
		time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// loadDotEnv populates os.Environ from a simple KEY=VALUE file baked into
// the image. Real environment variables (e.g. BLUEPRINT_TOKEN injected from
// a Secret) always win -- this only fills in values that aren't already set.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func httpJSON(method, url, token string, body any) (int, map[string]any, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, nil
}

func mintAndRegister(apiBase, blueprintID, token, agentImage string) (string, error) {
	for attempt := 1; attempt <= registerMaxAttempts; attempt++ {
		logf("attempt %d/%d: minting agent id", attempt, registerMaxAttempts)

		status, mintBody, err := httpJSON("POST", apiBase+"/graph/agents", token, map[string]any{})
		if err != nil || status != 201 {
			logf("mint failed (status=%d err=%v), retrying in %s", status, err, registerRetryDelay)
			time.Sleep(registerRetryDelay)
			continue
		}
		agentID, _ := mintBody["agentId"].(string)
		if agentID == "" {
			logf("mint response had no agentId (%v), retrying", mintBody)
			time.Sleep(registerRetryDelay)
			continue
		}
		logf("minted agent id=%s", agentID)

		status, _, err = httpJSON("POST", fmt.Sprintf("%s/blueprint/%s/register", apiBase, blueprintID), token,
			map[string]any{"agentId": agentID, "image": agentImage})
		if err != nil || status != 200 {
			logf("register call failed (status=%d err=%v), retrying", status, err)
			time.Sleep(registerRetryDelay)
			continue
		}
		logf("registered agent id=%s against blueprint=%s", agentID, blueprintID)
		return agentID, nil
	}
	return "", fmt.Errorf("failed to register after %d attempts", registerMaxAttempts)
}

func deregister(apiBase, token, agentID string) {
	for attempt := 1; attempt <= deregisterAttempts; attempt++ {
		status, _, err := httpJSON("POST", fmt.Sprintf("%s/agents/%s/deregister", apiBase, agentID), token, map[string]any{})
		if err == nil && status == 200 {
			logf("agent id=%s deregistered successfully", agentID)
			return
		}
		logf("attempt %d/%d: deregister returned status=%d err=%v, retrying", attempt, deregisterAttempts, status, err)
		time.Sleep(deregisterRetryWait)
	}
	// Deliberate: don't block shutdown forever over a failed cleanup call.
	// Log loudly for alerting and let the pod terminate -- the new agent is
	// already live regardless of this outcome.
	logf("WARNING: failed to deregister agent id=%s after %d attempts -- needs alerted manual cleanup in UBS", agentID, deregisterAttempts)
}

func main() {
	loadDotEnv("/.env")

	apiBase := strings.TrimRight(os.Getenv("UBS_API_BASE"), "/")
	blueprintID := os.Getenv("BLUEPRINT_ID")
	token := os.Getenv("BLUEPRINT_TOKEN")
	agentImage := os.Getenv("AGENT_IMAGE")

	logf("starting up: blueprint=%s image=%s api=%s", blueprintID, agentImage, apiBase)

	agentID, err := mintAndRegister(apiBase, blueprintID, token, agentImage)
	if err != nil {
		logf("FATAL: %v -- refusing to start workload", err)
		os.Exit(1)
	}
	healthy.Store(true)

	// Own health endpoint for the readinessProbe -- replaces the exec
	// "cat /tmp/registered" marker-file check from the hook-based design.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			if healthy.Load() {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not registered"))
		})
		if err := http.ListenAndServe(":8080", mux); err != nil {
			logf("FATAL: health server crashed: %v", err)
			os.Exit(1)
		}
	}()

	cmd := exec.Command("nginx", "-g", "daemon off;")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		logf("FATAL: failed to start workload: %v", err)
		os.Exit(1)
	}
	logf("workload started (pid=%d), agent %s is live and serving", cmd.Process.Pid, agentID)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case sig := <-sigCh:
		logf("received signal %s -- failing readiness and deregistering agent %s before shutdown", sig, agentID)
		healthy.Store(false)
		deregister(apiBase, token, agentID)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-waitCh
		logf("workload exited cleanly, shutdown complete")
	case err := <-waitCh:
		logf("FATAL: workload exited unexpectedly: %v", err)
		os.Exit(1)
	}
}
