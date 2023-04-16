package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ridge/must/v2"
	"github.com/tiktoken-go/tokenizer"
	"zombiezen.com/go/sqlite/sqlitemigration"
)

const openaiURL = "https://api.openai.com"

type completionRequestBody struct {
	Model  string
	Prompt string
	Suffix string
	Stream bool
}

type completionResponseBody struct {
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	}
}

const timeFmt = `2006-01-02 15:04:05.000`

func reqPrint(r *http.Request, prefix string, fmt string, args ...any) {
	log.Printf(prefix+"%s %21s "+fmt, append([]any{
		time.Now().UTC().Format(timeFmt),
		r.RemoteAddr,
	}, args...)...)
}

func logInfo(r *http.Request, fmt string, args ...any) {
	reqPrint(r, "INF ", fmt, args...)
}

func logError(r *http.Request, fmt string, args ...any) {
	reqPrint(r, "ERR ", fmt, args...)
}

func proxyRequest(w http.ResponseWriter, r *http.Request, client *http.Client, key string, pool *sqlitemigration.Pool) {
	if r.Method != http.MethodPost {
		logError(r, "Unexpected method %q", r.Method)
		http.Error(w, "Only POST requests are supported", http.StatusBadRequest)
		return
	}
	if r.URL.RawQuery != "" {
		logError(r, "Unexpected query %q", r.URL.RawQuery)
		http.Error(w, "Query parameters are not supported", http.StatusBadRequest)
		return
	}

	// We do not use request's context, as we want to count requests that were aborted by the client too.
	// Instead we use a new context with a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := mustGetDB(ctx, pool)
	defer pool.Put(conn)

	reqKey := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	userID, userName, userFound, err := findUserByKey(conn, reqKey)
	if err != nil {
		logError(r, "Failed to find user by key: %v", err)
		http.Error(w, "Failed to find user", http.StatusInternalServerError)
		return
	}
	if !userFound {
		logError(r, "User not found by key %q", reqKey)
		http.Error(w, "Invalid API key", http.StatusUnauthorized)
		return
	}

	projectName := r.Header.Get("X-Project")
	if projectName == "" {
		projectName = "<default>"
	}

	projectID, err := getProjectID(conn, userID, projectName)
	if err != nil {
		logError(r, "Failed to get project ID for user %q (ID=%d), project %q: %v", userName, userID, projectName, err)
		http.Error(w, "failed to find project", http.StatusInternalServerError)
		return
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		logError(r, "Failed to read request body for user %q (ID=%d), project %q (ID=%d): %v", userName, userID, projectName, projectID, err)
		http.Error(w, "failed to read request body", http.StatusInternalServerError)
		return
	}

	var crb completionRequestBody
	if err := json.Unmarshal(requestBody, &crb); err != nil {
		logError(r, "Failed to parse request body for user %q (ID=%d), project %q (ID=%d): %v", userName, userID, projectName, projectID, err)
		http.Error(w, "failed to parse request body", http.StatusBadRequest)
		return
	}

	if crb.Stream {
		logError(r, "Streaming is not supported, requested by user %q (ID=%d), project %q (ID=%d)", userName, userID, projectName, projectID)
		http.Error(w, "streaming responses are not yet supported", http.StatusBadRequest)
		return
	}

	_, err = tokenizer.ForModel(tokenizer.Model(crb.Model))
	if err != nil {
		logError(r, "Invalid model %q requested by user %q (ID=%d), project %q (ID=%d): %v", crb.Model, userName, userID, projectName, projectID, err)
		http.Error(w, "failed to find model "+crb.Model, http.StatusBadRequest)
		return
	}

	modelID, err := getModelID(conn, crb.Model)
	if err != nil {
		logError(r, "Failed to get model ID for model %q, requested by user %q (ID=%d), project %q (ID=%d): %v", crb.Model, userName, userID, projectName, projectID, err)
		http.Error(w, "failed to get model "+crb.Model, http.StatusInternalServerError)
		return
	}

	logInfo(r, "Proxying. user %q (ID=%d), project %q (ID=%d), model %q (ID=%d)", userName, userID, projectName, projectID, crb.Model, modelID)

	req := must.OK1(http.NewRequestWithContext(ctx, http.MethodPost, openaiURL+"/v1/chat/completions", bytes.NewReader(requestBody)))
	req.Header = r.Header.Clone()
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)

	// Network failures etc.
	if err != nil {
		logError(r, "Failed to proxy request for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
		http.Error(w, fmt.Sprintf("Failed to read response from OpenAI: %v", err), http.StatusBadGateway)
		return
	}

	defer resp.Body.Close()

	h := w.Header()
	for k, vs := range resp.Header {
		h.Del(k)
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if resp.StatusCode == http.StatusOK {
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			logError(r, "Failed to read response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
			http.Error(w, "failed to read response", http.StatusBadGateway)
			return
		}

		var crespb completionResponseBody
		if err := json.Unmarshal(responseBody, &crespb); err != nil {
			logError(r, "Failed to parse response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
			http.Error(w, "failed to parse response", http.StatusBadGateway)
			return
		}

		logInfo(r, "200 response read. user %q (ID=%d), project %q (ID=%d), model %q (ID=%d), tokens %d", userName, userID, projectName, projectID, crb.Model, modelID, crespb.Usage.TotalTokens)

		if err := saveUsage(conn, modelID, projectID, crespb.Usage.TotalTokens); err != nil {
			logError(r, "Failed to save usage for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d), tokens %d: %v", userName, userID, projectName, projectID, crb.Model, modelID, crespb.Usage.TotalTokens, err)
		}

		if _, err := w.Write(responseBody); err != nil {
			logError(r, "Failed to write response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
		}

		logInfo(r, "200 response sent. user %q (ID=%d), project %q (ID=%d), model %q (ID=%d)", userName, userID, projectName, projectID, crb.Model, modelID)
		return
	}

	if _, err := io.Copy(w, resp.Body); err != nil {
		logError(r, "Failed to write response body for user %q (ID=%d), project %q (ID=%d), model %q (ID=%d): %v", userName, userID, projectName, projectID, crb.Model, modelID, err)
	}

	logInfo(r, "Error response sent. %s, user %q (ID=%d), project %q (ID=%d), model %q (ID=%d)", resp.Status, userName, userID, projectName, projectID, crb.Model, modelID)
}

func serve(pool *sqlitemigration.Pool, listenURL string) {
	openaiKey := os.Getenv("OPENAI_KEY")
	client := &http.Client{}

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		proxyRequest(w, r, client, openaiKey, pool)
	})

	if err := http.ListenAndServe(listenURL, nil); err != nil {
		log.Fatalf("failed to listen on %s: %v", listenURL, err)
	}
}
